# Azure DevOps, per person — the admin's row, the member's sign-in, and the moment a run asks for more

This is the mock round for the Azure DevOps surfaces of 0.7.10 — the design gate before any console
code (owner law: the mock is UI source of truth; canon strings are app strings). The model is decided
by the workstream A design plan (§§1–10) and corrected by live measurement against a real Microsoft
Entra tenant (`FINDINGS.md`, F-LIVE-1 to F-LIVE-4). Nothing here is open for re-design, only for
drawing. **§7.2–§7.8 are FROZEN** (2026-09-22); the
drawing-level calls Q1–Q11 are resolved in §9, with the owner's answers in Adjudication.

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
every product string in the mock matches §7 byte-for-byte (checked mechanically, see §7's freeze note), and §7.2 onwards is exactly two columns,
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

### 0.1 Where the connection comes from — decided after the first mock round

**The credential is acquired at Wardyn sign-in, not by a separate errand.** When an enabled `entra`
row exists in the login tenant, the Wardyn sign-in's own authorization request carries the Azure
DevOps scopes, so a person who signs in to Wardyn is already connected. **Measured, F-LIVE-5:** one
authorization-code round trip requesting `openid profile offline_access` alongside the Azure DevOps
resource's `vso.*` scopes returns, in a single response, an `id_token` audienced to the application,
an `access_token` audienced to `499b84ac-1321-427f-aa17-267ca6975798`, and a `refresh_token`. The
whole of this section rests on that measurement rather than on an inference from F-LIVE-1. With directory-wide
administrator consent there is no extra screen at all; without it, Microsoft's consent screen appears
once, at that person's first sign-in after the row was turned on. Every `vso.*` scope this design uses
is user-consentable (F-LIVE-2), so the administrator's consent is an option that removes a prompt, not
a requirement that gates the feature.

The dedicated connect flow **survives only as a fallback**, for four named causes: the person signed
in before the row existed; consent is needed and has not been given; the connection ended (sessions
revoked, consent withdrawn); or they sign in to Wardyn through a different directory from the one the
Azure DevOps organisation uses. A "not connected" state always names which of the four it is.

**This has a cost, and the doc states it rather than burying it.** Plan §2 says "Wardyn login is
untouched — no change to `oidc.Session`, which still keeps no tokens." That is no longer true: the
login callback must now capture and store a refresh token for the person whenever an enabled `entra`
row is present. The blob is stored exactly as the fallback flow stores it
(`Secrets.For(subject)`, `wardyn-harness-ado-<rowid>-oauth`, the owned-blob read/store discipline),
and `oidc.Session` itself still carries no token — but the login *path* now has a credential-capture
side effect it did not have. **That is a design change to a security-relevant path and belongs in
the slice that implements it, with its own tests**, not in a UI round's footnote. The member-facing
consequence is the whole of §2.2.

Two things follow for the copy: the member's verb is **connect**, not "sign in" — they have already
signed in, and telling them to sign in again to a thing they are already signed in to is the
confusion this requirement exists to remove. And the common case renders **no button at all**.

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
  `modelaccess.go`'s; in a fallback state `CONNECT_ADO` sits where `AGENTS.SIGN_IN_AWS` sits, and in
  the common case nothing does.
- **The connect pane is `HarnessLoginPane`'s shape**, with a browser redirect instead of a device code
  (F-LIVE-4: device code is blocked by Conditional Access on a real tenant). It is the **fallback**
  path only (§0.1).
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

### 2.2 The connection

**The common case has no surface.** A person signs in to Wardyn, and their Getting started page says
`Azure DevOps · Connected through your sign-in`, success-toned, with no action line and no button.
Drawing a button here would invent the errand the requirement removes.

**The connected panel** shows the account, how it was connected, the organisation, the three
capability groups, and how it ends — "Renewed while you keep using it", with the two ways it stops
(the organisation ending sessions, the person being removed from the directory) and the fact that
signing in to Wardyn again reconnects it. Then the same three-row panel from §0, in the member's
words. It lives in two homes from one component: Getting started, and Settings beside the model
provider — a member checking months later what their runs can do looks in Settings, not in an
onboarding page (Q9).

**The fallback states each name their cause** (§0.1's four). A bare "not connected" with no reason is
the state that generates a support ticket.

**The consent screen is the fallback's dialog**, and the same words a person meets at a first sign-in
when the directory has not consented for everyone. It says what consent is — what Wardyn is able to
ask Azure DevOps for *at all* — and what it is not: what any one run may do. It lists the three
groups, names the application, and says where the permission can be withdrawn. Microsoft's own screen
follows; Wardyn does not restate or paraphrase it.

**Disconnect is honest about not being final.** Removing the stored connection is undone by the next
Wardyn sign-in; the confirm says so and points at the Microsoft page that actually withdraws it.

**The row surfaces consent, bounded by what Wardyn can actually observe.** Two things are knowable:
who holds a connection, and who Microsoft refused with a consent-required error. One thing is **not**:
how many people were shown a consent screen and accepted it — from the application's side a sign-in
that was consented to and one that never needed consent are identical, and consent-required is only
visible when it *fails*. So the field has three states — everyone who has signed in is connected;
`{n}` people aren't connected (with the refusal count that explains it); nothing seen yet — and the
honesty line says exactly which half Wardyn can see. The administrator action is offered in **every**
state, because it is the same action regardless.

An administrator-declared "I granted consent for the directory" flag was considered and rejected: a
self-declaration can go stale the moment someone edits the grant in Entra, Wardyn could never tell,
and §5 forbids rendering an unverifiable claim as a fact. Reading the directory through Microsoft
Graph was also rejected (Q10) — asking an operator to consent to a directory-read permission so the
product can talk about consent is a bad trade, and it would make Wardyn hold a permission it
otherwise never needs.

The Azure DevOps service principal may be **absent** from a tenant until someone creates it
(F-LIVE-3); that is an adoption-doc problem, not a console state. Device code is not used: it is
blocked by Conditional Access on a real tenant (F-LIVE-4).

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

**Most people never reach it**, because signing in to Wardyn connected them. It exists for §0.1's
four causes. Preflight carries a `git_credential` fact, so the rail says what it knows before anyone
presses Launch: connected (success), not connected (warning, plus "You'll be asked to connect when you
launch"), or expiring (warning, with what that means for a run that outlives it). Pressing Launch
while not connected opens the dialog rather than refusing; connecting relaunches the form exactly as
it stood. The raw 422s exist for the API path and are rendered verbatim if they ever reach a screen.

### 2.5 The after view

A run-detail widget: who the run acted as, what it started with, what it ended holding, and one row
per request with its outcome and decider. Then the honest pair — the scope list Azure DevOps actually
returned, which is **evidence, not a bound**, with the sentence that keeps it from being read as one;
and the note that Azure DevOps has its own record of every request that went through, under that
person's name. In `minted_pat` mode the first note is replaced by the token's name, its real scope
list and lifetime, and the time Wardyn revoked it — plus the one honest failure, a revoke that could
not run because the sign-in had ended.

### 2.6 States

- **Row** — per person + sign-in token; per person + minted token; shared token; consent in its three
  states; the six write refusals; saved-narrowed while runs are going.
- **Member** — connected through the Wardyn sign-in (the common case, no button); the connected panel
  in both homes; the four fallback causes; the consent screen; the disconnect confirm.
- **Access** — `live` · `expiring` · `expired_signin` · `not_configured` · `shared_expired` ·
  `not_applicable`, each crossed with where the connection came from (the Wardyn sign-in, or made
  separately) — which changes two chips and two remedies and **is not a seventh state**; plus
  shared-and-live (which reuses the existing "Provided by your admin" chip and deliberately makes no
  per-person claim).
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
today. On the member's Getting started card, **Connect Azure DevOps** is teal in the fallback states
only — the common case has no button at all, so it spends nothing. **On the capability card, Approve
is teal, except on a `policy_bypass` card where Deny is `destructive` and nothing is teal** — the
loudest button on the scariest ask should not be the one that says yes. (Q3 was ruled on the
three-button drawing; Q2 then replaced the control, so the ruling reads onto `Approve` rather than
onto `Allow once`. Same intent, one fewer button.) Zero teal on the after view.

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
9. **Never claim to know the directory's consent state, and never count consent prompts.** The row
   reports two observable things — who is connected, and who Microsoft refused for consent — and
   says which half it cannot see. It never renders "admin consent granted" as though it had read
   Entra, and it never shows "{n} people were prompted", because a successful authorization looks
   identical whether a consent screen appeared or was never needed.
10. **Never tell a member to sign in to something they are already signed in to.** The member's verb
    is *connect*. "Sign in to Azure DevOps" is the wrong sentence for a person whose Wardyn sign-in
    already covers it.
11. **Never say "the agent"**, or any model, plan or campaign word, in a product string. It is "the
   run", "this run", "runs".
12. **No member-facing string names another person's identity, a tenant GUID, a client GUID or a
    secret name.** The card names the run's owner because the decider is deciding about them; it
    names nothing else.
13. **The shared-token row keeps its honesty.** `shared` is not deprecated, not warned about in red,
    and not silently migrated — it is described accurately and left alone.
14. **Three shipped sentences are rewritten in the same change that falsifies them** (§7.8): the
    `tool_call` "no backend component enforces this", the two "Wardyn can't expire or down-scope a
    PAT", and the `PAT · in-sandbox` chip (which reads from the broker switch rather than being
    renamed outright — "in-sandbox" is still true when the broker is off).

## 6. Where the model lives on the page

1. **`/providers`, the Git tab, the Azure DevOps row** — the existing row gains, under the existing
   lanes: `FIELD_CREDENTIAL_SOURCE` (`Segmented`), `FIELD_TOKEN_MODE` (`Segmented`, only when per
   person), `FIELD_TENANT` and `FIELD_CLIENT` (the second read-only, filled from the login
   application), then "What a run may do here" — `CEILING_TITLE` / `CEILING_HINT` beside
   `PROFILE_TITLE` / `PROFILE_HINT` as two capability lists (the wire capability name renders in mono
   beside the plain one in the **ceiling column only**, Q8) — then `CONSENT_*`, then
   `ENFORCEMENT_TITLE` and the three-row panel, and `ALWAYS_DENIED_*` as the plain note.
2. **Getting started, "What's set up for you"** — one more chip from the six states. In the common
   case that is the whole of it: no action line, no button. In a fallback state the cause line and
   `CONNECT_ADO` render where `SIGN_IN_AWS` sits.
2b. **The connected panel**, one component in two homes — Getting started, and Settings as a card
   between Model provider and SSH keys (Q9).
3. **The live run** — the capability card in `live-approvals.tsx`, above the terminal.
4. **`/approvals`** — the same card, plus `Open run`, plus the decided table and the ended-run state;
   `TOOL_CALL_NOTE` replaces the screen's `tool_call` sentence.
5. **New Run rail** — the `git_credential` preflight chip and its sub-line; the dialog on Launch;
   the relaunch toast.
6. **Run detail** — a new widget, `AFTER_TITLE` / `AFTER_LEAD`, the request table, and the two
   honest notes.

## 7. Canonical strings — FROZEN

A backticked substring inside a string (a host, a ref, a wire value, a field) renders `font-mono`; the
frozen string is plain text and the parser strips backticks. A `{placeholder}` is substituted by the
caller. The namespace is `ADO`.

**Frozen 2026-09-22.** Every row from §7.2 on is now canon: the implementation transcribes it
verbatim into `ui/src/app/lib/ado-entra-copy.ts` and does not retype copy from anywhere else. The
mock draws these strings byte-for-byte, and `docs/design/ado-entra-mock/index.html` is checked
against this section rather than being a second source of truth. A change to a string here is a
change to the module and its test, in the same commit.

### 7.1 Reused canon — referenced, never re-frozen

| Key | Lives in | String |
|---|---|---|
| `PROVIDERS.TITLE` / `LEAD` | `workspace-providers-copy.ts` | Workspace providers / Where work can come from, and how big it can get. Enable a git provider to bound which repositories a run may clone; set the storage ceilings every run and every drive is held to. |
| `S.GIT_FOOTER` | `settings/connection-cards.tsx` | Only the GitHub App lane keeps its token outside the sandbox — a PAT or SSH key enters it for the clone, then is wiped. Public repos clone with no credential at all. |
| Settings Model provider card | `settings-screen.tsx` | Model provider / Agent runs need one. Governed commands don't. |
| The two sentences §7.8 replaces | `approvals.tsx`, `secrets.tsx` | No backend component enforces a tool_call approval. · Wardyn cannot expire or down-scope a PAT. — quoted so the replacement can be diffed against the exact bytes |
| `/approvals` header | `approvals.tsx` | Approvals / What runs asked for, and what was decided. — the screen's existing header, drawn for context. The implementation renders whatever ships there; this round does not re-freeze it. |
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
| Not connected, at run create (422, `reason: git_credential`) | `ADO_422.*`, `runs_create_validate.go` | git_credential: you are not connected to Azure DevOps — connect and start the run again |
| Connection ended, at run create (422, `reason: git_credential`) | `runs_create_validate.go` | git_credential: your Azure DevOps connection ended — connect and start the run again |
| Connection doesn't cover the run's baseline, at run create (422, `reason: git_credential`) | `scmaccess.go` | git_credential: your Azure DevOps connection doesn't cover the access this run needs — connect and start the run again |
| Repository outside the row's org (403, proxy) | `ADO_REFUSE.*`, `proxy/ado_gate.go` | this run may only reach {org} on Azure DevOps |
| Token creation attempted (403, proxy) | `proxy/ado_gate.go` | creating or revoking Azure DevOps tokens is refused for every run |
| Capability refused after a decision (403, proxy) | `proxy/ado_gate.go` | {capability} was denied for this run |
| Org policy refuses a mint | `ADO_PAT.*`, `api/ado_pat.go` | your organization's policy refuses this token: {policy} |

### 7.2 `ADO` — the provider row

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
| `SOURCE_PER_USER_HINT` | Each person's runs act as them. Turning this on connects everyone who signs in to Wardyn — they have nothing to set up. Azure DevOps applies each person's own permissions, and removing someone from your directory ends it. |
| `FIELD_TOKEN_MODE` | Token |
| `TOKEN_BEARER` | Their sign-in |
| `TOKEN_MINTED` | One Azure DevOps mints per run |
| `TOKEN_MODE_HINT` | Their sign-in is renewed in the background and never stored anywhere a run can read. A minted token is bounded by Azure DevOps itself, and exists there until Wardyn revokes it. |
| `TOKEN_MODE_SHARED_HINT` | There is one token and it is the one you stored. This choice appears when each person signs in. |
| `FIELD_TENANT` | Directory (tenant) ID |
| `TENANT_HINT` | The Microsoft Entra directory your organisation signs in to. |
| `FIELD_CLIENT` | Application (client) ID |
| `CLIENT_HINT` | Filled from the application this console signs in with. Wardyn can only tie an Azure DevOps sign-in to the person's own Wardyn session when both use one application. |
| `CONSENT_TITLE` | Consent |
| `CONSENT_ALL_CONNECTED` | Everyone who has signed in is connected |
| `CONSENT_ALL_CONNECTED_BODY(connected, total)` | {connected} of {total} people who have signed in are connected to Azure DevOps, and nobody has been refused for consent. |
| `CONSENT_SOME_MISSING(n)` | {n} person isn't connected / {n} people aren't connected |
| `CONSENT_SOME_MISSING_BODY(connected, total, refused)` | {connected} of {total} people who have signed in are connected to Azure DevOps. {refused} were refused by Microsoft for consent and haven't connected since. |
| `CONSENT_NOTHING_SEEN` | Nothing seen yet |
| `CONSENT_NOTHING_SEEN_BODY` | Nobody has signed in since this row was turned on, so there is nothing to report yet. |
| `CONSENT_GRANT_TITLE` | Grant it for everyone |
| `CONSENT_GRANT_BODY(directory)` | In Microsoft Entra, open this application's API permissions and choose “Grant admin consent for {directory}”. After that nobody is asked, including people who haven't signed in yet. |
| `CONSENT_HONESTY` | Wardyn can tell you who is connected, and who Microsoft refused for consent. It cannot tell you whether someone saw a consent screen and accepted it — from here, a sign-in that was consented to and one that never needed consent look identical. It does not read your directory's settings and cannot consent on your behalf. |
| `CAPS_TITLE` | What a run may do here |
| `CAPS_LEAD` | Two lists, and the difference between them matters: the ceiling is what a run may ever ask for, the defaults are what it starts with. |
| `CEILING_TITLE` | Capability ceiling |
| `CEILING_HINT` | What a run may ever ask for. Anything not ticked here is refused outright — nobody can approve it, not the person and not you. |
| `PROFILE_TITLE` | Every run starts with |
| `PROFILE_HINT` | What a run has before anyone decides anything. Everything else on the ceiling is held at the proxy until you or the person who started the run allows it. |
| `PROFILE_OFF_CEILING` | Not on the ceiling, so it can't be a default. |
| `PROFILE_READ_ONLY_NOTE` | Read-only is the honest default. A run that only reads never interrupts anyone; a run that starts able to push never asks. |
| `ALWAYS_DENIED_TITLE` | Always refused, on every Azure DevOps row |
| `ALWAYS_DENIED_BODY` | Creating or revoking tokens, service hooks, and installing extensions — and any Azure DevOps write Wardyn cannot name. None of them are on the ceiling, none can be approved, and no change here allows them. |
| `MINTED_EXPOSURE_TITLE` | A minted token is more exposed than a sign-in |
| `MINTED_EXPOSURE_BODY` | It exists at Azure DevOps under that person's name until Wardyn revokes it — at the end of the run, or on the next sweep if this Wardyn was stopped mid-run. A sign-in is never written anywhere and needs no cleanup. Choose this when you need Azure DevOps itself to bound the run. |
| `MINTED_POLICY_NOTE` | Your organisation may refuse token creation, or cap how long a token may live, by policy. If it does, runs on this row are refused with the policy named, and nothing falls back to a wider token. |
| `MINTED_REVOKE_NOTE` | Azure DevOps does not promise that revoking a token ends a connection already open with it. Revoking is what stops the next request, not necessarily the one in flight. |
| `SAVED_NARROWED_RUNS(n, capability)` | {n} run was granted {capability} and no longer holds it — its next request for it is refused. / {n} runs were granted {capability} and no longer hold it — their next request for it is refused. |

### 7.3 `ADO` — the enforcement panel

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
| `ENF_TOKEN_MEMBER_LABEL` | The connection's own reach |
| `ENF_TOKEN_MEMBER` | It covers everything you consented to for Azure DevOps — Wardyn doesn't make it smaller. That is why the check above exists, and why every one of these is recorded. |

### 7.4 `ADO` — capability labels

Q8: on the provider row's **ceiling column only**, the wire capability name renders in mono beside the
plain label (`Read` `read`, `Push` `code_write`, `Branch policies` `policy_admin`, and so on) — the
administrator authoring `capability_ceiling` by hand is the one person who needs the mapping. It
renders in the defaults column nowhere, and outside the row nowhere.

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
| `CAP_TOKENS` | Create an Azure DevOps token |

### 7.5 `ADO` — the member's connection and the six access states

The chip and the remedy vary with **where the connection came from** — the Wardyn sign-in, or a
separate connect. That is a second fact on the same row, not a seventh state (§2.6).

| Key | String |
|---|---|
| `ACCESS_LIVE_ORG` | Azure DevOps · Connected through your sign-in |
| `ACCESS_LIVE_SEPARATE` | Azure DevOps · Your connection |
| `ACCESS_EXPIRING` | Azure DevOps · Expiring |
| `ACCESS_EXPIRING_ACTION(ts)` | Your Azure DevOps connection ends at {ts}. Reconnecting takes one click. |
| `ACCESS_EXPIRED` | Azure DevOps · Disconnected |
| `ACCESS_EXPIRED_ORG_ACTION` | Your Azure DevOps connection ended. Runs that clone from it are refused until you connect again — signing out of Wardyn and back in does it too. |
| `ACCESS_EXPIRED_SEPARATE_ACTION` | Your Azure DevOps connection ended. Runs that clone from it are refused until you connect again. |
| `ACCESS_NOT_CONNECTED` | Azure DevOps · Not connected |
| `ACCESS_NEEDS_CONSENT` | Azure DevOps · Needs your consent |
| `ACCESS_SHARED_LIVE` | Azure DevOps · Provided by your admin |
| `ACCESS_SHARED_EXPIRED` | Azure DevOps · Your admin's credential expired |
| `ACCESS_SHARED_EXPIRED_ACTION` | Your admin's Azure DevOps token expired — ask them to replace it. There is nothing for you to connect. |
| `CAUSE_ROW_IS_NEWER` | You signed in to Wardyn before your admin turned Azure DevOps on, so your sign-in doesn't cover it yet. Connect now, or sign out and back in — either works. |
| `CAUSE_CONSENT_NEEDED` | Microsoft needs you to allow Wardyn to reach Azure DevOps as you. It's one screen, once. |
| `CAUSE_ENDED` | Your Azure DevOps connection ended — your organisation ended your sessions, or the consent was withdrawn. Runs that clone from it are refused until you connect again. |
| `CAUSE_OTHER_DIRECTORY` | You sign in to Wardyn through a different directory from the one this Azure DevOps organisation uses, so your Wardyn sign-in can't connect you. Connect separately instead. |
| `CONNECT_ADO` | Connect Azure DevOps |
| `CONNECT_AGAIN` | Connect again |
| `CONNECT_DIALOG_TITLE` | Connect Azure DevOps |
| `CONNECT_DIALOG_BODY(org)` | You'll go to your organisation's Microsoft sign-in and come straight back. After that your runs reach `{org}` as you, instead of through one account shared by everyone. |
| `CONNECT_CONSENT_BODY` | Microsoft may ask you to allow it once. What you allow is what Wardyn is able to ask Azure DevOps for at all. What any one run may actually do is smaller, and Wardyn holds it there: |
| `CONNECT_APP_NOTE(app)` | The application asking is {app} — the same one you signed in to this console with. You can withdraw this at any time from your Microsoft account's My Apps page; doing so stops your runs reaching Azure DevOps. |
| `CONNECT_CTA` | Continue to Microsoft |
| `CONNECT_POPUP_BLOCKED` | Your browser blocked the popup. |
| `GROUP_STARTS_WITH` | Starts with |
| `GROUP_CAN_ASK` | Can ask you for |
| `GROUP_NEVER` | Never |
| `PANEL_CONNECTED_AS` | Connected as |
| `PANEL_CONNECTED_AS_HINT` | Your runs act as this account, and Azure DevOps records them under it. |
| `PANEL_HOW` | How |
| `PANEL_HOW_ORG` | Your Wardyn sign-in |
| `PANEL_HOW_ORG_HINT` | Your admin turned this on for the organisation, so signing in here connected you. There is nothing to set up. |
| `PANEL_HOW_SEPARATE` | You connected it |
| `PANEL_HOW_SEPARATE_HINT` | Your Wardyn sign-in doesn't cover Azure DevOps, so this is a connection you made yourself. |
| `PANEL_ORG` | Organisation |
| `PANEL_ENDS` | Connection ends |
| `PANEL_ENDS_RENEWED` | Renewed while you keep using it |
| `PANEL_ENDS_HINT` | Wardyn renews it in the background. If your organisation ends your sessions, or you're removed from the directory, it stops — and signing in to Wardyn again reconnects it. |
| `PANEL_CARD_OPEN` | What your runs may do here |
| `PANEL_CARD_SUMMARY(start, n)` | Starts with {start} · can ask for {n} more |
| `DISCONNECT_CTA` | Disconnect Azure DevOps |
| `DISCONNECT_CONFIRM_TITLE` | Disconnect Azure DevOps? |
| `DISCONNECT_CONFIRM_BODY` | Runs you start after this can't reach Azure DevOps. Runs already going keep the connection they started with. Signing in to Wardyn again reconnects you — to stop that, withdraw the permission from your Microsoft account's My Apps page. |
| `ACCESS_SHARED_NOTE` | Rendered when the row uses a shared token and that token works. It is deliberately not a per-person claim: the run does not act as this person. |

A ref's policy summary on the card ("Protected: 2 reviewers required, build must pass") is rendered
from what Azure DevOps reports about that ref. It is not frozen here, because it is the forge's
description of the forge's own configuration and Wardyn does not author it.

`ACCESS_LIVE_ORG` renders with **no action line and no button** — that is the point of the whole
surface. `ACCESS_NOT_CONNECTED` never renders alone: it always carries one of the four `CAUSE_*`
lines. The member-facing verb is **connect** throughout, because "sign in" names something they have
already done (§5 #10).

### 7.6 `ADO` — the capability request card

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
| `REQ_WHAT_CODE_WRITE(repo, person)` | the run pushes this branch to {repo}, as {person}. |
| `REQ_BLAST_CODE_WRITE(repo)` | commits, and creating or moving branches no policy protects, anywhere in {repo}. Not a protected branch, not completing a pull request, not changing a policy — each of those asks separately. |
| `REQ_WHAT_POLICY_BYPASS(ref, person)` | the run moves {ref} past the policy protecting it, as {person}. |
| `REQ_BLAST_POLICY_BYPASS(repo)` | moving any policy-protected branch in {repo}, and completing a pull request with its policies bypassed. |
| `REQ_WHAT_POLICY_ADMIN(ref, person)` | the run lowers the reviewer count on {ref}, as {person}. |
| `REQ_BLAST_POLICY_ADMIN(repo)` | creating, changing and deleting branch policies anywhere in {repo} — including the ones that would hold back its own pushes. |
| `REQ_WHAT_PR(pr, person)` | the run completes pull request {pr}, as {person}. |
| `REQ_BLAST_PR(repo)` | opening, updating, commenting on and completing pull requests anywhere in {repo}. Completing one past its own policies is a separate ask. |
| `REQ_HELD(thing)` | The {thing} may be waiting at the proxy for a short time; approving lets it through now or the next time the run asks. |
| `REQ_SCOPE_READOUT(scope)` | Scope: {scope} |
| `REQ_APPROVING_ONCE(thing)` | Approving lets this one {thing} through. The next one asks again. |
| `REQ_APPROVING_RUN(thing)` | Approving lets every {thing} from this run through until it ends. Nothing carries to the next run. |
| `REQ_DENYING(thing)` | Denying refuses this {thing} for the rest of the run, unless this kind of change is later allowed for the whole run. |
| `REQ_SCOPE_ONCE_HINT(thing)` | This one {thing} goes through. The next one asks again. |
| `REQ_SCOPE_RUN_HINT(thing)` | Every {thing} from this run goes through until it ends. (default) |
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
| `REQ_UNCLASSIFIED_BODY` | An Azure DevOps write Wardyn doesn't recognise is refused rather than guessed at, because the card couldn't tell you truthfully what you'd be allowing. There is no setting that changes this — if a tool you need keeps hitting it, tell your admin what it was trying to do. |
| `REQ_CONSENT_CHIP` | Needs your Microsoft consent |
| `REQ_CONSENT_SOURCE(ts)` | Azure DevOps · you allowed this at {ts} · still held |
| `REQ_CONSENT_BODY` | Microsoft needs your consent before Azure DevOps lets this run use this access. Reconnecting asks Microsoft for it — you'll see a consent screen, and nothing else changes. The run's request stays held meanwhile. |
| `REQ_CONSENT_CTA` | Connect Azure DevOps |
| `REQ_CONSENT_OTHER_BODY(person)` | You allowed it, but only {person} can give Microsoft the extra permission it needs — the run acts as {person}, and consent is theirs to give. They've been shown this on their Getting started page. |
| `REQ_NOT_YOURS_CHIP` | Not yours to decide |
| `REQ_NOT_YOURS_BODY(person)` | Only {person}, who started this run, or an admin can answer this. |
| `REQ_TIMEOUT_BODY(thing)` | Nobody answered within four minutes, so the {thing} was refused and the run was told. It can ask again. |
| `REQ_REAUTH_CHIP` | Connection ended |
| `REQ_REAUTH_TITLE` | Your Azure DevOps connection ended mid-run |
| `REQ_REAUTH_BODY` | This run's next Azure DevOps request is held for up to four minutes while you reconnect. Reconnect and it goes through on its own — the run doesn't have to start over. |
| `LIST_ENDED_CHIP` | This run has ended |
| `LIST_ENDED_BODY` | There's nothing to allow — the request went away with the run. It's here so you can see what it asked for. |
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

**Post-freeze corrections (S10 round 2 — owner-delegated to the lead, 2026-09-22):**

- `REQ_DENYING` was frozen before #414 taught `answerADOCapability` that a deny STICKS for the rest
  of the run — the same canonical request is refused by naming the denied approval, regardless of the
  scope it was denied at (`adoCapDeniedForRunRefusal`). "The run keeps going and may ask again" was
  true when a deny only ever bound the one held request; it is not true now. The row above also names
  the ESCAPE HATCH the sticky refusal has: `adoStanding` still honours a later `run`-scoped APPROVAL of
  the same capability, so "unless this kind of change is later allowed for the whole run" is the
  accurate whole of it — a sticky deny is not permanent, an `Approve` at `run` scope lifts it. Because
  a deny's SCOPE choice no longer changes what a DENY itself does, the `Scope: {scope}` readout
  (`REQ_SCOPE_READOUT`) is not shown next to Deny — only next to Approve, where once/run still means
  something.
- `REQ_HELD` / `REQ_HELD_EXPIRED` (§10.3) claimed "up to four minutes" as if the card could always
  tell a still-held request from one whose hold already lapsed server-side and was refused. It cannot:
  no expiry timestamp reaches the client (see ado-capability-card.tsx's `stillHeld` comment), so its
  240s window is a CLIENT-SIDE ESTIMATE, not a read fact. Both rows are reworded to stop promising a
  number the card cannot verify, while still being honest that approving works either way (it lets a
  genuinely-still-held request through, or raises a fresh one if the old hold already lapsed).
- `REQ_CONSENT_BODY` said "You allowed it, but…", which is only true when the consent gap follows a
  person's own approval of an escalation. It is equally reachable when a capability is CONSENTED-yet-
  ungranted at dispatch (the run started with it in `sn.Capabilities`, and Entra later refuses the
  redemption) — nobody "allowed" anything in that path; the run simply asked. The reworded sentence is
  true in both cases. It DROPS the `{capability}` parameter (no capability name reaches the wire scope
  either way — see this card's own doc comment), so this is a plain string as of round 2, not a
  function.
- `REQ_CONSENT_CTA` said "Allow and continue", which named a different act than the page it lands
  on (issue #458): the destination is `ado-connection.tsx`'s Settings card, whose own CTA has always
  read `CONNECT_ADO`, "Connect Azure DevOps". Reworded to match, and the destination gained an anchor
  (`#azure-devops`) so the two consent-chain doors (this row and the mid-run sign-in row) land ON the
  card rather than at the top of a five-card page — see §10.7.

### 7.7 `ADO` — the launch door and the after view

| Key | String |
|---|---|
| `PREFLIGHT_LIVE(person)` | Azure DevOps · connected as {person} |
| `PREFLIGHT_MISSING` | Azure DevOps · not connected |
| `PREFLIGHT_MISSING_SUB` | You'll be asked to connect when you launch. |
| `PREFLIGHT_EXPIRING(ts)` | Azure DevOps · your connection ends at {ts} |
| `PREFLIGHT_EXPIRING_SUB` | A run going when it ends holds its next Azure DevOps request for up to four minutes while you reconnect. Reconnecting now avoids that. |
| `LAUNCH_ANYWAY` | Launch anyway |
| `LAUNCH_DIALOG_TITLE` | Connect Azure DevOps first |
| `LAUNCH_DIALOG_BODY(org)` | This workspace clones from `{org}`, and your runs there act as you. Connect once and this form comes back exactly as you left it. |
| `RELAUNCH_TOAST_TITLE` | Connected to Azure DevOps. |
| `RELAUNCH_TOAST_BODY` | Your run is as you left it — launch when you're ready. |
| `LAUNCH_WARNING_EXPIRING(ts)` | Your Azure DevOps connection ends at {ts}. After that this run holds its next Azure DevOps request for up to four minutes while you reconnect. |
| `AFTER_TITLE` | Azure DevOps access |
| `AFTER_LEAD` | What this run asked for, what it was given, and who decided. |
| `AFTER_ACTED_AS` | Acted as |
| `AFTER_STARTED_WITH` | Started with |
| `AFTER_ENDED_HOLDING` | Ended holding |
| `AFTER_ONCE_NOTE(capability)` | {capability} was allowed once, for one request, and never joined the run's standing set. |
| `AFTER_SCOPES_TITLE` | What the connection itself covered |
| `AFTER_SCOPES_BODY(list)` | Azure DevOps issued this run a token covering: {list}. |
| `AFTER_SCOPES_HONESTY` | That is what the token carried, not what the run was allowed to do. Wardyn's own check is what held it to the decisions above; a Microsoft Entra token can't be issued narrower than what the person has consented to. |
| `AFTER_MINTED_TITLE` | What the token itself covered |
| `AFTER_MINTED_BODY(name, caps, from, to, revoked)` | Azure DevOps minted `{name}` for this run, limited to {caps} and to {from}–{to}, and Wardyn revoked it at {revoked}. Outside those, Azure DevOps refused the request itself. |
| `AFTER_REVOKE_FAILED_TITLE` | This run's Azure DevOps token hasn't been revoked |
| `AFTER_REVOKE_FAILED_BODY(name)` | Your connection had ended, so Wardyn couldn't revoke `{name}`. It will be revoked the next time you sign in. You can also delete it yourself under your Azure DevOps personal access tokens. |
| `AFTER_FORGE_TITLE` | Azure DevOps has its own record |
| `AFTER_FORGE_BODY(person)` | Every request that went through appears in your organisation's audit log and in the repository's push history, under {person} — not under Wardyn, and not under a shared account. |

### 7.8 Sentences this design falsifies — rewritten in the same change

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

## 9. Owner question list — Q1–Q9 RESOLVED at the gate

The owner reviewed the first mock round and ruled on all nine. Each is recorded here with its ruling;
the Adjudication section below carries the owner's answers as given.

**Q1 — the ceiling/profile layout. RESOLVED (a):** two capability lists side by side, each with its
meaning above it. Clearer than two checkboxes per row. Drawn as (a); (b) is not drawn.

**Q2 — the decision control. RESOLVED (b), the losing recommendation:** the card uses the **shipped
`Approve` + caret + `Deny`** control, not three separate buttons. One card in the console with a
control of its own is an inconsistency an enterprise reviewer notices, and consistency outweighs the
legibility three buttons buy. The consequence sentences survive the change — they render **inline
under the control**, where they change with the selected scope, rather than under three buttons. The
scope menu offers `Once` and `This run` and shows `Until…` and `Always` disabled with their reason;
Deny takes the same scope, one caret governing both, as it does today. The three-button drawing stays
in the mock as the labelled losing variant — the three button labels and their consequence lines in
that block are **retired copy, deliberately not frozen**, and are the only product-looking strings on
the page that are absent from §7.

**Q3 — teal on the card. RESOLVED (a), and re-read onto the new control:** `Approve` is teal on an
ordinary card; on a `policy_bypass` card **nothing is teal and Deny is `destructive`**. The ruling was
given against the three-button drawing where the teal was `Allow once`; Q2 then replaced the control,
so the same intent now lands on `Approve`. Same rule, one fewer button.

**Q4 — `SOURCE_PER_USER` wording. RESOLVED (a), reworded:** the label stays "Each person signs in",
but the hint is rewritten so the row reads as connecting everyone through the organisation's sign-in
rather than as each person doing setup (§7.2).

**Q5 — the hold's length in the copy. RESOLVED (a):** name it in prose as well as showing the
countdown — and on the **waiting** card, not only in the refusal. `REQ_HELD` now says "for up to four
minutes while you answer".

**Q6 — `CAP_UNCLASSIFIED` on the row. RESOLVED (b), the safer ship:** there is **no** "Anything Wardyn
does not recognise" ceiling checkbox in 0.7.10. An unclassifiable write is always refused. The refused
card survives and its copy now says there is no setting that changes it, and asks the person to tell
their admin what the tool was doing. This can be added later without unwinding anything, which is why
it is the right way round.

**Q7 — the shared-token row's enforcement panel. RESOLVED (a):** the shared row keeps its own panel.
That row is where the honesty matters most, because it is what every install runs today.

**Q8 — capability vocabulary. RESOLVED (b), on the row only:** the wire capability name renders in
mono beside the plain label, in the **ceiling column only** (§7.4). Nowhere else.

**Q9 — where the member's connected panel lives. RESOLVED (b):** Getting started **and** Settings,
one component in two homes.

### Q10 and Q11 — RESOLVED at the same gate

**Q10 — how the row learns about consent. RESOLVED: observation, and no Graph read.** Asking an
operator to consent to a directory-read permission so the product can talk about consent is a bad
trade, and it would leave Wardyn holding a permission it otherwise never needs.

**The ruling also corrected the drawing.** The first version showed "{n} were asked to consent at
sign-in". Wardyn cannot know that: from the application's side a successful authorization looks
identical whether a consent screen appeared or consent was never needed, and consent-required is only
visible when it **fails** (`AADSTS65001`). That count is now removed. The field draws the two things
Wardyn can observe — who is connected, and who was refused for consent — and the honesty line names
the half it cannot see. An administrator-declared flag was considered as the honest source for the
stronger statement and rejected: it can go stale the moment the grant is edited in Entra, Wardyn
could never tell, and §5 #9 forbids rendering an unverifiable claim as a fact. The administrator
action renders in every state, because it is the same action either way.

**Q11 — `Disconnect` for someone connected through their Wardyn sign-in. RESOLVED: keep it**, with
the honest confirm. A control that exists and says plainly that the next Wardyn sign-in will
reconnect you — and points at the Microsoft page that actually withdraws the permission — is more
useful than no control, and it matches what the product can truthfully do.

## Adjudication

### Owner answers

Reviewed 2026-09-22. The mock was approved in shape; the open questions were delegated and answered
as recorded in §9: **Q1 (a) · Q2 (b) · Q3 (a) · Q4 (a, reworded) · Q5 (a) · Q6 (b) · Q7 (a) ·
Q8 (b, row only) · Q9 (b)**. The three-row enforcement panel, the buttonless refused states and §5's
never-claim list were confirmed as drawn.

**One new requirement was given at the same time, and it changed a surface rather than a wording:**
an administrator should be able to set this up once for the organisation and have it work for
everyone through their existing SSO. A person may have to allow Wardyn once; a person must not have
to set up their own Azure DevOps connectivity. §0.1 records the model that follows, §2.2 is rewritten
around it, and the member's verb changed from *sign in* to *connect* throughout.

**Q10 and Q11 were ruled at the same gate** — see §9. Q10's ruling also corrected the drawing: the
"{n} were prompted" count came out, because Wardyn cannot know it. **§7 is frozen as of 2026-09-22.**

**Carried back to the implementation, not settled here:** §0.1's last paragraph. Acquiring the
credential at sign-in means the login callback gains a credential-capture side effect, which plan §2
explicitly said it would not have. That is a change to a security-relevant path, and it needs its own
slice, its own tests and a cross-tier review. **That slice is now being built separately, with an
independent cross-tier review**; the sentence stays here because it belongs in the design record and
not only in a lane's commit message. It is not a copy decision and this document does not settle it.

### Round notes (author, 2026-09-22)

0. **§7.2–§7.8 were frozen at the 2026-09-22 gate.** The DRAFT marker sat in each heading rather
   than in each row, because a marker in the key cell would break `parseFrozenTables`' clone; the
   owner froze by deleting the word from the headings, which is what happened.
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
6. **The mock is one page, not three.** The providers round split its mock so no file passed the
   1000-line gate; that gate covers `.go`, `ui/src` and `scripts`, not documentation, and the owner
   reviews these states side by side. One page wins. It is now just over 1000 lines, which the gate
   does not police for documentation; if it grows much further, split it the way the providers round
   did rather than letting one file carry every surface.
7. **`.dec` survives in the stylesheet for the losing variant only.** Q2 retired it from every real
   card; it is kept so the rejected drawing still renders beside the accepted one.
8. **The consent field is a row field, not a docs footnote**, because it is the difference between a
   member seeing nothing and a member seeing one Microsoft screen — the only part of this the
   administrator can actually change.
9. **Nothing on the page says "agent".** The subject is "the run", throughout, including in the
   sentences that replace shipped copy.
10. **`CONNECT_POPUP_BLOCKED` was added to §7.5 after the freeze** (implementation review, #386): a
    popup a browser refuses to open needs a plain-link fallback wherever CONNECT_CTA's popup can be
    blocked, and the fallback line was shipping as three copies of hand-typed, unfrozen text before
    this row existed. One sentence, approved at the same gate as the rest of §7.5.

Not drawn, deliberately: a second forge, a second identity provider, multi-party approval, a
per-capability application registration, device code, a "tell my admin" action on the above-ceiling
card, and any change to how `pat` / `ssh` rows reach Azure DevOps today.

## 10. Addendum — capability-card additions (S10 round 2, lead-approved, added post-freeze)

An independent review of the first capability-card implementation found real strings the mock draws
in context but §7.2–§7.8's tables never gave their own row — chiefly the per-capability NOUN each
consequence sentence's `{thing}` plugs in (§7.6 draws "push"/"change"/"action" inline, State 5, never
as a keyed row) — and a few small fields (Ref class, a consent-card heading) the card needs and the
mock's own drawing doesn't isolate as text either. These rows are ADDED, not amended: §7.2–§7.8 stay
exactly as frozen 2026-09-22 above. Every row below is pinned by the same parser
(ado-entra-copy.test.ts, N3 round 3 — originally ado-capability-copy.test.ts, before that file
merged into this one's canon) that checks §7.4/§7.6/§7.8.

### 10.1 `ADO` — the consequence sentences' `{thing}`, per capability

The mock (§7.6 State 5, `docs/design/ado-entra-mock/index.html` ~492-606) draws four capabilities and
picks a different noun for each: `code_write` and `policy_bypass` both read "push" (they're both a
push, one past a policy and one not), `policy_admin` reads "change", `pr` reads "action". The other
five grantable capabilities (`read`, `repo_admin`, `build_execute`, `work_write`, `wiki_write`) are not
drawn in the mock; their nouns below are the lead's own extension, in the same register.

| Key | String |
|---|---|
| `CAP_THING_READ` | read |
| `CAP_THING_CODE_WRITE` | push |
| `CAP_THING_PR` | action |
| `CAP_THING_POLICY_ADMIN` | change |
| `CAP_THING_POLICY_BYPASS` | push |
| `CAP_THING_REPO_ADMIN` | change |
| `CAP_THING_BUILD_EXECUTE` | run |
| `CAP_THING_WORK_WRITE` | edit |
| `CAP_THING_WIKI_WRITE` | edit |

### 10.2 `ADO` — the ref-class field and the consent card's heading

`REQ_FIELD_REF_CLASS`/`REQ_REF_CLASS_PROTECTED` name the fact the canonical scope actually carries
(`ref_class: "protected"`) without inventing a ref name the wire scope does not have (`adoCapabilityScope`
carries no ref). `REQ_CONSENT_HEADING` titles the Entra-consent card (`credential_reauth` /
`entra_consent`) when it stands alone rather than paired with the escalation it blocked — see
ado-capability-card.tsx's own doc comment for why it is not paired.

| Key | String |
|---|---|
| `REQ_FIELD_REF_CLASS` | Ref class |
| `REQ_REF_CLASS_PROTECTED` | Protected by a branch policy |
| `REQ_CONSENT_HEADING` | Azure DevOps needs more access |

### 10.3 `ADO` — the hold's honest expiry (owner-delegated to the lead, 2026-09-22)

No expiry timestamp reaches the client for an Azure DevOps hold (unlike egress's HOLD_TIMEOUT_MS), so
the card can only ESTIMATE whether a request is still parked at the proxy from its own `requested_at`
— it cannot tell a still-held request from one whose hold already lapsed server-side and was refused.
`REQ_HELD`'s original "for up to four minutes" claimed a precision the card does not have; both rows
below say only what is actually true either way: approving works whether the hold is live (it lets the
parked request through) or already lapsed (it raises a fresh one the next time the run asks).

| Key | String |
|---|---|
| `REQ_HELD_EXPIRED(thing)` | No longer waiting — approving lets the {thing} through the next time the run asks. |

### 10.4 `ADO` — the decided badge

Reused, not re-frozen: `OUTCOME_ALLOWED_ONCE`/`OUTCOME_ALLOWED_RUN` are already §7.6 rows (the decided
LIST's own outcome column). Round 2 extends `approvalScopeBadge` (`wardyn/copy.ts`) to read them for a
decided Azure DevOps escalation too, so the /approvals and run-detail decided rows say "Allowed once" /
"Allowed for this run" rather than the lowercase `once`/`this run` egress badge shares with every other
kind. No new row: this section exists only to record the pointer.

### 10.5 `ADO` — the remaining mount-site strings (owner-delegated to the lead, 2026-09-22)

Round-2 review (N6): every string the card and its mount sites render was hardcoded somewhere rather
than pinned — a run-fetch error, the scope caret's two disabled labels, the live strip's consent
heading, and the Runs board's Azure DevOps sign-in chip. Moved here so the parity test covers them too.

| Key | String |
|---|---|
| `REQ_RUN_UNAVAILABLE` | Couldn't load this run — try again. |
| `SCOPE_UNTIL_LABEL` | Until… |
| `SCOPE_ALWAYS_LABEL` | Always |
| `STRIP_HEADING_CONSENT` | Azure DevOps sign-in needed — sign in to let this run's Azure DevOps access through |
| `WAITING_ADO_MINE` | Waiting for your Azure DevOps sign-in |
| `WAITING_ADO_OWNER` | Waiting for the owner's Azure DevOps sign-in |

`WAITING_ADO_MINE`/`WAITING_ADO_OWNER` back `waitingAdoConsent(n, mine)` (`lib/reauth-waiting-copy.ts`)
exactly the way `REAUTH_ROW`'s AWS strings back `waitingReauth` — the count suffix (`· {n-1} more
waiting`) is composed client-side, same as that function, and is not itself a frozen string (a number
is not canon).

### 10.6 `ADO` — the mid-run sign-in card (owner-delegated to the lead, 2026-09-22)

A refresh token that dies mid-run, or a Conditional Access policy that wants the person present, now
HOLDS the run's Azure DevOps request on a `credential_reauth` row (`mechanism: entra_signin`,
`reason: signin`) until the person signs in again — the §7.6 "connection ended mid-run" state. The card
reuses `REQ_REAUTH_CHIP`/`REQ_REAUTH_TITLE` and `CONNECT_ADO`, but not `REQ_REAUTH_BODY`: that row
promises "up to four minutes", and this hold's bound is the operator's
`WARDYN_CREDENTIAL_REAUTH_TIMEOUT` (ten minutes by default). The body below claims no number. The board
chip and the strip heading (`WAITING_ADO_*`, `STRIP_HEADING_CONSENT`) already say "Azure DevOps
sign-in" and are true for this row as they stand.

| Key | String |
|---|---|
| `REQ_REAUTH_HELD_BODY` | This run's Azure DevOps request is held while you sign in again. Sign in and it goes through on its own — the run doesn't have to start over. If the hold runs out first, its next request goes through once you have. |
| `REQ_REAUTH_OTHER_BODY(person)` | Only {person} can sign in again — the run acts as {person}. Its Azure DevOps requests go through once they have. |

### 10.7 `ADO` — the not-applicable Settings card and the owner fallback (issue #458, owner-approved mock packet 6a, 2026-09-22)

Go grades an admin-token or local-mode caller `not_applicable` (`scmaccess.go:137-139`,
`isMechanism := subject == ""`) — reachable, not the theoretical case §7.5's original comment assumed:
an admin who configures a per-user Azure DevOps row while signed in with the admin token sees exactly
this state in their own Settings. `ado-connection.tsx`'s card returned early only for the absent-row
case (`state === ""`); every other unrecognised state, `not_applicable` included, fell through every
branch and rendered a title over an empty body. `NOT_APPLICABLE_BODY` is the one line that state gets
(Q458-1: a sentence, not a second chip vocabulary) — the Connected / Not connected / admin's-token-
expired states above are unchanged.

`REQ_OWNER_FALLBACK` promotes the capability card's own hardcoded `"the run's owner"` — the text
`REQ_NOT_YOURS_BODY(person)` falls back to when a run's `created_by` hasn't loaded — to a frozen row,
per §5's rule that no literal outside this module speaks for the design.

| Key | String |
|---|---|
| `NOT_APPLICABLE_BODY` | This sign-in is an admin token, not a person, so it has no Azure DevOps connection of its own. Each person's own connection carries their runs. |
| `REQ_OWNER_FALLBACK` | the run's owner |
