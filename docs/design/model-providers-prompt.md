# Model providers — the provider editor (key and endpoint kinds), and a refusal's own door

This is the mock round for two console issues in the multi-provider epic #551 that are still
`needs-mock`: **#537** (MP-19a, the provider editor for the key and endpoint kinds, with "Use with")
and **#543** (MP-24, a refusal opens its own provider's door; the failure block and the approvals
reauth card name the provider). It is the design gate before either lane writes console code (owner
law: the mock is UI source of truth; canon strings are app strings).

The model is decided and is not open here:
- the multi-provider design, `~/wardyn-archive/mock-08/multi-provider-design.md` §2, §5.2, §5.7,
  §5.8, §5.10 (owner-approved as packet 0, 2026-09-22);
- mock packets A–E (`mp-packet-A.html` … `mp-packet-E.html` in the same folder), each approved as
  recommended on 2026-09-22, with one change to E: sentences that start "this run's …" start with a
  capital letter;
- the user-types rename (user-types packet B, approved 2026-09-23): "Member view" is "User view", and
  E's "Open in member view" is "Open in user view".

Packets B and E already froze most of the words for these two issues. This round draws every state
the issues name in the console's look, adds the few strings the packets left unwritten, and asks
five questions (§9).

Static mock: `docs/design/model-providers-mock/` — `index.html` (the packet: screens, states,
decisions), `provider-editor.html` (#537), `door-cards.html` (#543), `canon.html` (every string, one
row each, with its id). They share `mock.css`. Open any file in a browser; no build, no network.

---

## 1. What exists today

- **No provider editor.** Settings shows `ModelProviderCard` ("Model provider · Agent runs need one.
  Governed commands don't."). Per-harness model access is the Agents tab's mechanism radio,
  credential toggle and start URL (`workspace-providers-copy.ts` `AGENTS`). #536 replaces the card
  with the Model providers list (packet A); #537 is the editor that list opens.
- **The server half of the editor exists.** `GET/PUT /model-providers` and `validateModelProviders`
  are on `main` (`internal/api/model_providers.go`): one whole-block PUT with `If-Match`, and a 400
  per rule (`mp400*`), which E6 renders verbatim.
- **The failure block offers AWS only.** `run-detail/failure-block.tsx` shows the server's
  `failure_hint` under "What happened", and a `Sign in to AWS` door when the ending's `mechanism` is
  `bedrock_sso` and the viewer owns the run. A Codex CLI run refused over its OpenAI key has no door;
  before 0.7.8 it opened AWS (#146 defect 2).
- **The reauth card** (`screens/approvals.tsx`) is titled `REAUTH_TITLE` "Sign in to AWS again — this
  run is paused" and has a shared-lane hint (`REAUTH_ROW.sharedMemberHint`) that 0.8 retires.
- **The New Run rail** answers a 422 `model_credential` by opening the AWS door once and launching
  again (`new-run-rail.tsx`, `credentialRefused`), and shows the server sentence on its launch-error
  line.

## 2. The model this round draws

### 2.1 The provider editor (#537)

- **Configuration only.** No key or token field anywhere; each kind states what each person
  provides.
- **Kind step** `What kind of model provider?`. Picking a kind moves on. Until #538 ships, the step
  lists the three kinds #537 builds (Q1).
- **Common:** `Name`, prefilled with the kind label (packet A, QA-5).
- **Key kinds** (Anthropic, OpenAI): `Route through a gateway (optional)`, open (QB-3), with the
  vendor host in the hint.
- **Endpoint kind:** `Base URL` (required), `Auth header` (default `Authorization`), `Value format`
  (default `Bearer {token}`).
- **Use with:** a checkbox per harness (QB-1). A new provider starts with every compatible harness
  ticked. An incompatible one is disabled with the catalog's reason verbatim (E5). Under a tick:
  `Model` (optional), and for the endpoint kind `Path` (required). A harness's settings are collapsed
  unless a required field is empty (QB-2).
- **Save:** disabled at once, spinner after ~200ms, no label swap (CONSOLE-RULES §7). Saved: the
  editor closes to the list with a toast (Q4). Refused: `This provider can't be saved as written`
  over the server's 400 verbatim; the form keeps what was typed (E6).
- **Edit:** title = the provider's name, kind label as a neutral chip; the id is never shown (Q3);
  `Remove` (outline) on the left of the footer.
- **Remove:** E7 confirm (destructive button), or E8 blocked while the provider is still an agent's
  default. Turning it off stays on the list (packet A, A7).
- **Rule 8 (E9):** a change to the base URL, route-through URL, a path or the auth header deletes
  everyone's credential for the provider. Save asks first, naming the count (QB-4); with nobody
  connected, it saves with no confirm (Q2).

### 2.2 Refusals and doors (#543)

- **The run's provider**: a neutral chip `Model provider · {name}` in the run header's chip bar, with
  ` (removed)` once the provider is deleted (Q5).
- **Failure block**: the server's sentence under "What happened", then:
  - the run's owner, in the User view: the door of the run's own provider — `Sign in to AWS`,
    `Sign in to Claude`, `Add your key` or `Add your token` — and its note;
  - anyone else, and every run in the Admin view: `It ran on {owner}'s own credential — only they can
    reconnect it.` and no door; on the admin's own run, `Open in user view`;
  - off, not available, not granted: the sentence only.
- **Which door** (§5.8): keyed by the refusal's own `provider` (the #146 ruling), never by what is
  selected on screen. On New Run the rail opens that door (AWS: on its own, then relaunches); for
  off / not available / not granted / a choice required it focuses the picker instead.
- **Reauth card**: title `AWS sign-in needed for this run` (Q146-2), a new line `Model provider ·
  {name}` under it, then today's banner and hints. The shared-lane hint retires.
- The audit readers (`failure-block.tsx`, `lib/api/audit.ts`) read `provider` and `kind` instead of
  `mechanism` — no visible change beyond the above.

## 3. Do not design (out of scope this round)

- The Bedrock and Claude subscription kinds, E3 and E4 (#538).
- The Model providers list (#536), the Agents tab (#539), Your model connections and the shell strip
  incl. B9 (#540, #541), the New Run picker's own states (#542).
- The doors' inner states — opening, device code, saved, cancelled (#544). Only their first line is
  drawn, to show which one opens.
- The `If-Match` 412 on a racing save, and a harness turned off on the roster while ticked: neither
  issue describes them.
- Any org-wide or shared credential (D3), and failover between providers (D1).

## 4. Design system

Same token block and idioms as `docs/design/workspace-providers-mock/mock.css`, copied into this
folder's `mock.css` (the four rungs, no ad-hoc sizes — CONSOLE-RULES §3), plus a
`prefers-color-scheme` default, the per-harness `Use with` block, a required marker, a spinner, the
failure block, the approvals card and phone-width rules.

- **One teal per surface.** The editor: `Save`. E9's confirm: `Save`. The failure block: the door.
  The rail: `Launch run`. The reauth card: `Sign in to AWS`.
- **Destructive** only on E7/E8's `Remove`; the edit footer's `Remove` is outline.
- **Back-out is ghost:** `Cancel` everywhere.
- **Mono for literals only:** URLs, hosts, paths, header names, model ids, wire kinds, the server's
  400 bodies stay plain text inside a red note (as the workspace-providers mock drew them).
- **Required** is a `*` sibling of the label: Base URL and Path only. Name is prefilled and optional.

## 5. Hard canon constraints

1. **Server sentences are verbatim.** The refusal (#532), the 400s (`mp400*`) — the console adds a
   heading at most, never rewording.
2. **Packets A–E strings are not re-frozen here.** §7.2 lists them as drawn; a change to one is a
   change to its packet.
3. **A door is keyed by the refusal's `provider`**, never by the selection on screen or the roster.
4. **No door for anyone but the run's owner, and none in the Admin view.** An admin's sign-in lands in
   the admin's namespace and can never serve another person's run.
5. **No string implies a substitute provider.** Every refusal ends "Wardyn does not substitute a
   different model provider."
6. **No member-facing string names another person's credential value, a secret name, or a full
   URL.** The destination is a host only (D7).

## 6. Where it lives on the page

- **Editor** — a `lg` dialog opened from Settings → Model providers (`Add model provider`, or a row).
  Top to bottom: title; what each person provides; Name; the kind's fields; `Use with`; footer.
- **Run header** — the chip bar in `run-detail-summary-header.tsx`, after the state chips.
- **Failure block** — `run-detail/failure-block.tsx`, unchanged layout: eyebrow, sentence, door row,
  audit row.
- **Rail** — `new-run-rail.tsx`, the Credentials section and the launch-error line.
- **Reauth card** — `screens/approvals.tsx`, the provider line under the title.

## 7. Canonical strings

A `{placeholder}` is filled by the caller. Backticked substrings render mono. §7.3 and §7.4 are
DRAFT until the owner answers §9; everything else is already frozen or shipped.

### 7.1 Reused canon — shipped in `ui/src` today

| Key | Lives in | String |
|---|---|---|
| `INTEGRATIONS.X_KEY_CODEX` | `lib/integrations.ts` | Codex CLI speaks the OpenAI API only — an Anthropic key can't drive it. Not a setting. |
| `INTEGRATIONS.X_OPENAI_CLAUDE` | `lib/integrations.ts` | Claude Code speaks the Anthropic API only — an OpenAI key can't drive it. Not a setting. |
| `HAPPENED` | `run-detail/failure-block.tsx` | What happened |
| `OPEN_AUDIT` | `run-detail/failure-block.tsx` | Open audit trail |
| `AGENTS.SIGN_IN_AWS` | `lib/workspace-providers-copy.ts` | Sign in to AWS |
| `MODEL_ACCESS_RUN_DOOR.NOTE` | `wardyn/model-access-copy.ts` | Sign in here. This run stays failed — relaunch it from the run header. |
| `MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA` | `wardyn/model-access-copy.ts` | Sign in to AWS — for this failed run |
| `MODEL_ACCESS_BANNER.DIALOG_TITLE` | `wardyn/model-access-copy.ts` | Sign in to AWS |
| `REAUTH_ROW.label` | `wardyn/model-access-copy.ts` | AWS sign-in needed |
| `REAUTH_ROW.hint` | `wardyn/model-access-copy.ts` | If a model call is waiting on your sign-in, the run continues after you sign in; if it already failed, relaunch it. |
| `REAUTH_ROW.notYoursHint(owner)` | `wardyn/model-access-copy.ts` | Waiting on {owner}'s AWS sign-in — only they can complete it. |
| `APPROVAL_BANNER_LABEL.what` | `wardyn/copy/approvals.ts` | What you're approving: |
| rail section / button | `new-run/new-run-rail.tsx` | Credentials · Launch run |

**Retired by #543:** `REAUTH_ROW.sharedMemberHint` ("This run uses the shared AWS sign-in — ask your
admin to sign in again.") — no shared sign-in exists in 0.8 (D3).

### 7.2 Frozen by packets A–E, drawn here unchanged

| Key | Packet | String |
|---|---|---|
| `MODEL_PROVIDERS.ADD_CTA` | A | Add model provider |
| `MODEL_PROVIDERS.KIND.ANTHROPIC_API_KEY` | A | Anthropic API key |
| `MODEL_PROVIDERS.KIND.OPENAI_API_KEY` | A | OpenAI API key |
| `MODEL_PROVIDERS.KIND.CUSTOM_ENDPOINT` | A | Your own endpoint |
| `MODEL_PROVIDERS.KIND.ANTHROPIC_SUBSCRIPTION` | A | Claude subscription |
| `MODEL_PROVIDERS.KIND.BEDROCK` | A | Amazon Bedrock |
| `PROVIDER_EDITOR.KIND_TITLE` | B | What kind of model provider? |
| `PROVIDER_EDITOR.CLAUDE_IMAGE_MISSING` | B | Claude subscriptions need the Claude Code sign-in image, which this install hasn't built yet. See Operations → Claude sign-in image. |
| `PROVIDER_EDITOR.PROVIDES_KEY` | B | Each person adds their own key. |
| `PROVIDER_EDITOR.PROVIDES_TOKEN` | B | Each person adds their own token. You set how it is sent. |
| `PROVIDER_EDITOR.NAME` | B | Name |
| `PROVIDER_EDITOR.NAME_HINT` | B | What people see when they choose it. |
| `PROVIDER_EDITOR.ROUTE_THROUGH` | B | Route through a gateway (optional) |
| `PROVIDER_EDITOR.ROUTE_THROUGH_HINT(host)` | B | Send requests to your gateway instead of {host}. Each person's key goes with them, and they are told where it goes. |
| `PROVIDER_EDITOR.BASE_URL` | B | Base URL |
| `PROVIDER_EDITOR.BASE_URL_HINT` | B | https only. Wardyn sends this provider's requests here. |
| `PROVIDER_EDITOR.AUTH_HEADER` | B | Auth header |
| `PROVIDER_EDITOR.VALUE_FORMAT` | B | Value format |
| `PROVIDER_EDITOR.USE_WITH` | B | Use with |
| `PROVIDER_EDITOR.MODEL` | B | Model |
| `PROVIDER_EDITOR.MODEL_HINT` | B | Leave empty for the agent's own default. |
| `PROVIDER_EDITOR.PATH` | B | Path |
| `PROVIDER_EDITOR.PATH_HINT_CLAUDE` | B | Where your endpoint serves the Anthropic Messages API for Claude Code, e.g. /anthropic. |
| `PROVIDER_EDITOR.PATH_HINT_CODEX` | B | Where your endpoint serves the OpenAI Responses API for Codex CLI, e.g. /v1. |
| `PROVIDER_EDITOR.CANCEL` | B | Cancel |
| `PROVIDER_EDITOR.SAVE` | B | Save |
| `PROVIDER_EDITOR.REMOVE` | B | Remove |
| `PROVIDERS.SAVE_REFUSED_TITLE_ONE` | B | This provider can't be saved as written |
| `PROVIDER_EDITOR.DELETE_TITLE(name)` | B | Remove {name}? |
| `PROVIDER_EDITOR.DELETE_BODY` | B | Runs that chose it are refused until they choose another. Everyone's tokens for it are deleted. |
| `PROVIDER_EDITOR.DELETE_BLOCKED(name, harness)` | B | {name} is the default for {harness} — choose another default first. |
| `PROVIDER_EDITOR.ADDRESS_TITLE(name)` | B | Change where {name} sends requests? |
| `PROVIDER_EDITOR.ADDRESS_BODY(n)` | B | The tokens {n} people added were given for the old address, so they are deleted. Everyone who connected adds theirs again. |
| `RAIL_PROVIDER.LABEL` | C | Model provider |
| `RAIL_PROVIDER.STATIC(name)` | C | Model provider · {name} |
| `RAIL_PROVIDER.OPTION(name, what, state)` | C | {name} — your {what} · {state} |
| `CONNECTIONS.SIGN_IN_CLAUDE` | D | Sign in to Claude |
| `CONNECTIONS.ADD_KEY` | D | Add your key |
| `CONNECTIONS.ADD_TOKEN` | D | Add your token |
| `RUN_FACTS.PROVIDER(name, removed)` | E | Model provider · {name}[ (removed)] |
| `MODEL_ACCESS_RUN_DOOR.NOTE_KEY` | E | Add it here. This run stays failed — relaunch it from the run header. |
| `MODEL_ACCESS_RUN_DOOR.NOT_OWNER(owner)` | E | It ran on {owner}'s own credential — only they can reconnect it. |
| `DOOR.FOR(name)` | E | For {name} |
| `KEY_DOOR.TITLE(what, name)` | E | Add your {what} for {name} |
| `KEY_DOOR.DESTINATION(host)` | E | Sent to {host} |
| `REAUTH_ROW.PROVIDER(name)` | E | Model provider · {name} |
| `REAUTH_TITLE` | E (changes today's) | AWS sign-in needed for this run |
| `CONSOLE_VIEW.OPEN_IN_USER` | E, renamed by user-types B | Open in user view |

### 7.3 `PROVIDER_EDITOR` — new in this round (DRAFT)

| Key | String |
|---|---|
| `SAVED_TOAST` | Provider saved. |
| `DELETE_BODY_KEY` | Runs that chose it are refused until they choose another. Everyone's keys for it are deleted. |
| `ADDRESS_BODY_ONE` | The token 1 person added was given for the old address, so it is deleted. They add it again. |
| `ADDRESS_BODY_KEY(n)` | The keys {n} people added were given for the old address, so they are deleted. Everyone who connected adds theirs again. |
| `ADDRESS_BODY_KEY_ONE` | The key 1 person added was given for the old address, so it is deleted. They add it again. |

### 7.4 `MODEL_ACCESS_RUN_DOOR` — new in this round (DRAFT)

| Key | String |
|---|---|
| `SIGN_IN_CLAUDE_ARIA` | Sign in to Claude — for this failed run |
| `ADD_KEY_ARIA` | Add your key — for this failed run |
| `ADD_TOKEN_ARIA` | Add your token — for this failed run |

### 7.5 Server sentences — reference, rendered verbatim, frozen by their server issue

| Key | Owner | String |
|---|---|---|
| refusal | #532 (design §2.6) | This run's model provider is {name}, and {state} — {remedy} Wardyn does not substitute a different model provider. |
| state | #532 | you are not signed in to AWS for it · you are not signed in to Claude for it · you have not added your key for it · you have not added your token for it · it is turned off · it is not available to {harness} · you are not granted it |
| state + remedy, not the owner | #532 (packet E) | it runs on {owner}'s own credential — only they can reconnect it. |
| remedy | #532 | connect it from Getting started in the console, or from the banner the console shows on every page. · choose another model provider, or ask your admin. |
| 400s | `internal/api/model_providers.go` (shipped) | the `mp400*` constants; E6 draws `mp400BaseURL`, `mp400Path`, `mp400Model` |

## 8. Where to apply (once implemented, out of scope this round)

- **#537:** a new `settings/model-provider-editor.tsx` opened from #536's list; strings in the
  multi-provider copy module the lanes create from §7.2–§7.3; client for `PUT /model-providers`
  from #535.
- **#543:** `run-detail/failure-block.tsx` (door keyed by `provider`/`kind`, the owner rule, the
  Admin-view rule), `lib/api/audit.ts` (read `provider`/`kind`, not `mechanism`),
  `run-detail-summary-header.tsx` (the chip), `new-run/new-run-rail.tsx` (the door per provider),
  `screens/approvals.tsx` + `wardyn/model-access-copy.ts` (`REAUTH_TITLE`, the provider line, the
  retired shared hint).
- Before calling either done: the unit tests beside each file, and `scripts/run-ui-e2e.sh` for the
  touched specs; #545 (MP-26) pins cases (a)–(e).

## 9. Owner question list

**Q1 — The kind step before #538 ships.** (a) three kinds until #538 adds Bedrock and Claude
subscription; (b) hold #537 until #538, so the step opens with all five. **Recommend (a)**: each
lane ships a step that works, and no in-between state goes undrawn. Drawn: (a), with (b) as a
variant.

**Q2 — E7 and E9 for keys, one person, and nobody.** Packet B wrote them for tokens only. Drawn: the
key sentences, the one-person sentences (§7.3), and no confirm when nobody has connected. **Recommend
as drawn.**

**Q3 — Editing, and the provider id.** Drawn: the title is the name, with the kind as a chip; the id
is made from the name at first save, fixed after, and never shown. The alternative shows the id
read-only under Name (a new label and hint). **Recommend as drawn**; the trade-off is that an admin
needing the id for `--model-provider` finds it outside the editor.

**Q4 — After Save.** (a) close to the list with `Provider saved.`; (b) stay open. **Recommend (a)**:
the list row shows the result, including the admin's own connection chip.

**Q5 — Where a run shows its provider.** (a) a neutral chip in the run header's chip bar; (b) a line
in the run summary. **Recommend (a)**: the header bar is on every run page at every width.

## Adjudication

### Owner answers

Owner approved mock packet 1 on 2026-09-25 (#537 provider editor, #543 refusals, failure block and
reauth card). All five decisions are approved **as drawn**:

1. **Q1 — kind step.** #537 lists only the three kinds it builds (Anthropic API key, OpenAI API
   key, Your own endpoint). #538 adds Amazon Bedrock and Claude subscription.
2. **Q2 — E7/E9 wording.** The confirms have key wording and one-person wording, and there is no
   confirm when nobody has connected yet — four new sentences (canon rows tagged New).
3. **Q3 — editing.** The title is the provider name with a kind chip. The provider id is derived
   from the name at first save, stays fixed, and isn't shown.
4. **Q4 — after Save.** The editor closes to the list with the toast "Provider saved."
5. **Q5 — the run's provider.** A neutral chip in the run header, "Model provider · {name}".

**Settled without asking**, accepted as listed: the user view rename, the `REAUTH_TITLE` change,
the required Path field, every compatible agent ticked by default, and accessible names.

Every string in `canon.html` is now the app's string byte for byte.

### Round notes (author, 2026-09-25)

1. **Packets B and E are the source**, extracted from `~/wardyn-archive/mock-08/mp-packet-B.html` and
   `mp-packet-E.html`; every string they froze is drawn unchanged, with E's capital-letter change and
   the user-types rename applied.
2. **Path is required in the console, optional on the server.** `validProviderPath` accepts an empty
   path (the base URL itself). The drawing follows QB-2; the lane may relax the marker without a
   copy change.
3. **The not-owner server sentence** (§7.5) is per viewer, while `failure_hint` is stored once per
   run. #532 has to compose it for the viewer, or the console has to pick it; the drawing follows
   packet E and leaves the mechanism to #532.
4. **The E6 bodies are real**: `mp400BaseURL` wrapping `validateOneLLMGateway`'s "must be https://"
   error, `mp400Path`, and `mp400Model`, with the provider id `corp-gateway` that Q3's rule gives.
