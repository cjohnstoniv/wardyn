# "Available to" on the stored policy, model provider, integration and base image editors

This is the mock round for **#923** (UT-7d, `needs-mock`), the last four families of the user-types
work (#622) that still lack their "Available to" control. It is the design gate before the lane
writes console code (owner law: the mock is UI source of truth; canon strings are app strings).

The model is decided and is not open here:
- the user-types design, `~/wardyn-archive/mock-08/user-types-design.md` §2.6 (rev 4);
- user-types packet A (`user-types-packet-a.html`, approved 2026-09-23): D1, "one 'Available to'
  on every resource", its drawing of the control on a git provider row, and its image hint;
- user-types packet B (approved 2026-09-22/23): "member" is "user";
- the model-providers packets A and B (approved 2026-09-22) and their mock
  (`docs/design/model-providers-mock/`, branch `design/551-providers-mock-packet`), whose provider
  editor this round adds one block to.

The control itself is #919's `AvailabilityControl` (`components/wardyn/availability-control.tsx`,
open PR, branch `feat/619-availability-console`). #923 reuses it; this round decides where it sits in
four editors, what differs per family, and draws every state once.

Static mock: `docs/design/available-to-mock/` — `index.html` (the packet: screens, decisions),
`editors.html` (the four families, a section each: they share one control), `states.html` (the
control's states, drawn once on a stored policy), `canon.html` (every string, one row each, with its
id). They share `mock.css`, copied from the model-providers mock. Open any file in a browser; no
build, no network.

---

## 1. What exists today

- **The server.** UT-10 (`feat/612-resource-availability`, not on `main`): `GET/PUT
  /permissions/availability/{kind}/{value}` (`securityOps`) reads and flips a per-value restricted
  bit; the "Only these" list is the allow rows in `capability_grants`, written through `POST/DELETE
  /permissions/grants`. Turning Only on with no allow row is a 400 (`availabilityOnlyEmptyMsg`). A
  kind not marked `restrictable` in `capKinds` is a 400 on both routes.
- **`capKinds` on `main`** marks `workspace`, `image`, `agent`, `integration`, `workspace_provider`
  restrictable. `policy` arrives with #818 (UT-11); `model_provider` with MP-6a. Until then a
  policy's control reads a 400, which is why #919 dropped its policy wiring.
- **The control** (#919): Everyone / Only these; audience chips with ×; an adder (User type / Group /
  User segmented, a field, Add); optional family hint and note; the last chip's × locked while Only
  is on; security admins and super admins only, absent (never an error) for anyone else; writes at
  once, never through the surrounding editor's draft or Save.
- **Stored policy**: `policies.tsx` `PolicyDetail`, a right-hand sheet any signed-in person can open;
  `Edit policy` is super-admin only (`Requires the admin role.`).
- **Model provider**: no editor on `main`. #537 builds it from the model-providers mock.
- **Integrations**: no per-row editor. Settings shows `ModelProviderCard` (the AI kinds as lanes);
  the GitHub App and git hosts are git provider rows on Workspace providers → Git providers.
  `capIntegration` gates only `req.IntegrationID` (the AI-provider integration a person names), tier
  1 only (`capabilities.go`).
- **Base images**: no console surface. `GET/POST /base-images` and `DELETE /base-images/{id}` exist
  (`operatorOnly`, `sources.go`); creating a workspace with a base image upserts a catalog row.
  `capImage` WIDENS and its value is the image reference: a person may launch a ref by name
  (`--image`, or their own workspace's image) only with a grant. An image an admin sets on an org
  workspace is not gated (`denyMemberSeededImage`).

## 2. The model this round draws

### 2.1 The control, every family (states.html)

- **Default: Everyone** for policies, model providers and integrations; **Admins only** for an image
  (design §2.6 defaults; Q2 for the words).
- **A subset**: write the list first (while Everyone, the rows change nothing), then choose Only
  these. Chips: a type by its name, a group as `{name} (group)`, a person by email (packet A).
- **None selected is not a state**: Only with nobody listed is the server's 400, rendered verbatim
  under the choice, which stays on Everyone. The last chip's × is locked while Only is on (#919);
  on an image the reason names Admins only (Q2).
- **Taking a type off** writes at once, with no confirm. Runs already going keep what they have
  (packet A's git provider line); the type's next launch is refused (answer 3 of §2.6) and the
  resource disappears from its lists (answer 1). The type editor's reverse view shows `Not available
  · only {types}`.
- **In flight**: disabled at once, spinner on Add after ~200ms, no label swap (CONSOLE-RULES §7).
- **Refusals**: an add the server refuses (e.g. an unknown user type) renders its sentence inline
  under the adder, not in a toast (CONSOLE-RULES §9). A failed read renders one line.
- **Who sees it**: security admins and super admins. On the policy sheet a security admin gets the
  live control and a disabled `Edit policy`; anyone else gets the sheet without the control.

### 2.2 Where it sits (editors.html)

- **Stored policy**: in the detail sheet, after UI apps and before View raw JSON. Kind `policy`, value
  = policy id.
- **Model provider**: in the provider editor, after Use with and above the footer. Kind
  `model_provider`, value = provider id. On a new provider the form asks (Q4).
- **Base image**: a new **Images** tab on Workspace providers (Q1): catalog rows, each with its ref and
  control; empty state; `Add image` (one field, the ref, plus the control). Kind `image`, value =
  the ref.
- **Integration**: no editor of its own (Q3).

## 3. Do not design (out of scope this round)

- The type editor's reverse view (UT-7a) and the person side (#922, #736): one row and one sentence
  each, only to show consequence.
- The kind-wide enforcement switch; an image restricted while that switch is on.
- Removing an image from the catalog; `If-Match` races; two catalog rows with the same ref.
- Secrets, egress hosts, drives, SSH keys and API tokens (their own issues).

## 4. Design system

`mock.css` is the model-providers mock's stylesheet, copied verbatim, plus a block for the control
(radio, chips with ×, the adder), the policy sheet, the Images row and the reverse-view row, all from
the same tokens.

- **One teal per surface.** The control has none of its own (it writes on change). Provider editor:
  `Save`. Images tab and its dialog: `Add image`. Policy sheet: none (`Edit policy` is outline).
- **Back-out is ghost:** `Cancel`.
- **Mono for literals only:** refs, ids, `--image`.
- **Required** is a `*` sibling of the label: the Add image field only.

## 5. Hard canon constraints

1. **Server sentences are verbatim** (§7.4). The console adds a heading at most.
2. **Packet strings are not re-frozen here.** A change to one is a change to its packet.
3. **Never merely hidden in the console** (design §2.6): the control is the write side; what a person
   is offered comes filtered from the server, and a named value is refused by the server.
4. **No "available to nobody".** Nothing in the console may empty an Only list.

## 6. Where it lives on the page

- **Policy sheet** — `screens/policies.tsx` `PolicyDetail`, between the UI apps field and the raw-JSON
  disclosure.
- **Provider editor** — #537's `settings/model-provider-editor.tsx`, a block after Use with.
- **Images tab** — `screens/providers/providers-screen.tsx` gains a fourth tab; a new
  `providers/images-tab.tsx` lists `GET /base-images` and posts `POST /base-images`.

## 7. Canonical strings

A `{placeholder}` is filled by the caller. Backticked substrings render mono. §7.3 is DRAFT until the
owner answers §9. Rows marked (PR #919) are in that open PR's `availability-copy.ts` but not on
`main` and were never put to the owner; they are listed as new here.

### 7.1 Reused canon — on `main` today

| Key | Lives in | String |
|---|---|---|
| `PERM.SUBJECT_GROUP` · `PERM.SUBJECT_USER` | `lib/permissions-copy.ts` | Group · User |
| `OPERATOR_ONLY_REASON` | `wardyn/copy.ts` | Requires the admin role. |
| `RESIDUAL_PREFIX` | `wardyn/copy.ts` | Doesn't stop: |
| `CC_META.CC2.label` · `doesntProtect` | `wardyn/cc-meta.ts` | Wall · A flaw in the sandbox software itself (rare); it's still not a fully separate machine. |
| `POLICY_UI_APPS.label` · `none` | `wardyn/copy/ui-apps.ts` | UI apps · None declared |
| sheet fields, disclosure, button | `screens/policies.tsx` | Policy ID · Barrier · Created · Updated · View raw JSON · Edit policy |
| `PROVIDERS.TITLE` · `LEAD` · `GIT_TITLE` · `STORAGE_TAB` | `lib/workspace-providers-copy.ts` | Workspace providers · Where work can come from, and how big it can get. Enable a git provider to bound which repositories a run may clone; set the storage ceilings every run and every drive is held to. · Git providers · Storage |
| `AGENTS.AGENTS_TITLE` | `lib/workspace-providers-copy.ts` | Agents |
| `INTEGRATIONS.X_KEY_CODEX` | `lib/integrations.ts` | Codex CLI speaks the OpenAI API only — an Anthropic key can't drive it. Not a setting. |
| `S.MODEL_TITLE` · `S.MODEL_LEDE` | `settings/connection-cards.tsx` | Model provider · Agent runs need one. Governed commands don't. (Q3's other option only) |

### 7.2 Frozen by approved packets, drawn here unchanged

| Key | Packet | String |
|---|---|---|
| `AVAILABILITY.LABEL` | UT-A | Available to |
| `AVAILABILITY.EVERYONE` | UT-A | Everyone |
| `AVAILABILITY.ONLY` | UT-A | Only these |
| `AVAILABILITY.ADMINS_ONLY` | UT-A (reverse view) | Admins only |
| `AVAILABILITY.ADD_PLACEHOLDER` | UT-A | Add a type, group or person… |
| `AVAILABILITY.CHIP_TYPE(name)` · `CHIP_GROUP(name)` · `CHIP_USER(email)` | UT-A | {name} · {name} (group) · {email} |
| `AVAILABILITY.IMAGE_HINT` | UT-A | Images are off for everyone until you list someone here. |
| `AVAILABILITY.PROVIDER_ONLY_HINT` (pattern, not drawn) | UT-A | Only these — people not listed can't add or run repositories from this organisation. Runs already going aren't affected. |
| `AVAILABILITY.PROVIDER_NOTE` (pattern, not drawn) | UT-A | Security admins can change this list from the type editor too. The organisation itself is edited only by super admins. |
| `USER_TYPES.GETS_TITLE(type)` | UT-A | {type} · What this type gets |
| `USER_TYPES.NOT_AVAILABLE` · `ONLY(types)` · `IMAGES` | UT-A | Not available · only {types} · Images |
| `MODEL_PROVIDERS.ADD_CTA` · `KIND.*` | MP-A | Add model provider · Anthropic API key · Your own endpoint |
| `PROVIDER_EDITOR.*` (as drawn) | MP-B | Each person adds their own key. · Each person adds their own token. You set how it is sent. · Name · What people see when they choose it. · Route through a gateway (optional) · Send requests to your gateway instead of {host}. Each person's key goes with them, and they are told where it goes. · Base URL · https only. Wardyn sends this provider's requests here. · Use with · Remove · Cancel · Save |

### 7.3 New in this round (DRAFT)

| Key | Q | String |
|---|---|---|
| `PERM.SUBJECT_USER_TYPE` (PR #919) | — | User type |
| `AVAILABILITY.ADD_CTA` (PR #919) | — | Add |
| `AVAILABILITY.HINT_USER_TYPE` (PR #919) | — | The user type's id, exactly as it appears on the User types screen. |
| `AVAILABILITY.HINT_GROUP` (PR #919) | — | A group or app-role name exactly as your identity provider sends it in the token. |
| `AVAILABILITY.HINT_USER` (PR #919) | — | An email address or the sign-in subject id. |
| `AVAILABILITY.REMOVE_ARIA(chip)` | — | Remove {chip} |
| `AVAILABILITY.LAST_AUDIENCE_LOCKED` (PR #919) | — | Choose Everyone before removing the last one, or nobody could use this. |
| `AVAILABILITY.LAST_AUDIENCE_LOCKED_IMAGE` | Q2 | Choose Admins only before removing the last one. |
| `AVAILABILITY.LOAD_FAILED` (PR #919) | — | Available to — couldn't load. |
| `AVAILABILITY.POLICY_ONLY_HINT` | Q5 | Only these — people not listed can't choose this policy for a run. Runs already going aren't affected. |
| `AVAILABILITY.POLICY_NOTE` | Q5 | Security admins can change this list from the type editor too. The policy itself is edited only by super admins. |
| `AVAILABILITY.MODEL_PROVIDER_ONLY_HINT` | Q5 | Only these — people not listed can't run with this provider, even from a workspace pinned to it. Runs already going aren't affected. |
| `AVAILABILITY.MODEL_PROVIDER_NOTE` | Q5 | Security admins can change this list from the type editor too. The provider itself is edited only by super admins. |
| `AVAILABILITY.IMAGE_NOTE` | Q5 | Security admins can change this list from the type editor too. The image list itself is edited only by super admins. |
| `AVAILABILITY.CREATE_PARTIAL_TITLE` | Q4 | Saved, but not who it's available to |
| `AVAILABILITY.CREATE_PARTIAL_EVERYONE` | Q4 | It is available to everyone until you set it again below. |
| `AVAILABILITY.CREATE_PARTIAL_IMAGE` | Q4 | It stays admins-only until you set it again below. |
| `IMAGES.TAB` | Q1 | Images |
| `IMAGES.LEAD` | Q1 | Images people may launch by name, with `--image` or as their own workspace's image. An image you set on an org workspace needs nothing here. |
| `IMAGES.ADD_CTA` | Q1 | Add image |
| `IMAGES.EMPTY_TITLE` | Q1 | No images listed |
| `IMAGES.EMPTY_BODY` | Q1 | Until you add one, only admins can launch an image by name. |
| `IMAGES.REF` | Q1 | Image |
| `IMAGES.REF_HINT` | Q1 | The exact reference people will type, e.g. ghcr.io/acme/dev-toolbox:1.4. |

### 7.4 Server sentences — reference, rendered verbatim, frozen by their server issue

| Key | Owner | String |
|---|---|---|
| `availabilityOnlyEmptyMsg` | UT-10 (#612) | Add at least one person, group or user type before choosing Only, or nobody could use this. |
| `accessUnknownUserType(id)` | UT-1 | The user type "{id}" doesn't exist. Create it under User types first. |
| kind not restrictable | UT-10 | "{kind}" can't be restricted to a list; only the resources people are offered can be. |
| the frozen refusal | #736 (AK-2), design §2.6 | {resource} isn't available to you as a {type}. Ask your admin. |
| model provider, not granted | #532 | This run's model provider is {name}, and you are not granted it — choose another model provider, or ask your admin. Wardyn does not substitute a different model provider. |

## 8. Where to apply (once implemented, out of scope this round)

- `lib/availability-copy.ts` (from #919): the §7.3 `AVAILABILITY.*` rows; chips and the remove
  label per §7.2 (#919 draws `User type: developer` today); an inline refusal under the adder instead
  of #919's toast; a family flag for Admins only on `image`.
- `screens/policies.tsx`: the control in `PolicyDetail`, after #818; the New policy form per Q4.
- #537's provider editor: the block after Use with, and Q4 on the new-provider form, after
  `model_provider` is restrictable.
- `screens/providers/`: the Images tab (Q1) and `lib/api` for `GET/POST /base-images`.
- Before calling it done: unit tests beside each file, and `scripts/run-ui-e2e.sh` for a Playwright
  pin per family (a listed type sees it; an unlisted one is refused with the frozen sentence).

## 9. Owner question list

**Q1 — Where a base image's list lives.** (a) a new Images tab on Workspace providers: the catalog's
rows, each with its control, plus Add image; (b) no page, only the type editor. **Recommend (a)**:
images are admins-only until someone is listed, so without a page nobody can see which are open and
to whom.

**Q2 — The first choice on an image.** (a) `Admins only` / Only these; (b) `Everyone` / Only these
like every family. **Recommend (a)**: an unlisted image is admins-only, so "Everyone" would be
false.

**Q3 — Integrations.** (a) no editor of their own: the AI kinds become model providers (their list is
on the provider editor) and git integrations are git provider rows (#919); (b) the control on
Settings → Model provider until MP-15. **Recommend (a)**: (b) is removed again within the release.

**Q4 — A new resource.** (a) the creation form asks, as §2.6 says: Save creates, writes the list,
turns Only on, and a refused list write leaves the editor open with `Saved, but not who it's available
to`; (b) no control on creation forms, the list is set after the first save. **Recommend (a)**, as
the approved design reads.

**Q5 — Each family's lines.** (a) policy and model provider get an only-these line and a note on the
git provider's pattern, and an image gets the note; (b) no family lines, as #919 draws agents and
workspaces. **Recommend (a)**: the only-these line is the one place the admin reads what taking
someone off does.

## Adjudication

### Owner answers

_(empty — the owner fills this at the gate)_

### Round notes (author, 2026-09-25)

1. **#919 diverges from packet A in two places**, both drawn here per the packet: the chips (#919:
   `User type: developer`; packet A: `Developer`) and the remove label. The adder is drawn per #919
   (a typed name alone can't tell a type from a group) with packet A's placeholder.
2. **"Runs already going aren't affected" is unverified for model providers.** The git provider line
   is approved; whether an in-flight run's model calls re-check `capModelProvider` is MP-6a's to
   confirm before the lane ships Q5's provider line.
3. **Q4's two writes are not atomic.** Between the create and the restriction write, a narrowing
   resource is available to everyone. On an image there is no gap, because an image starts
   admins-only.
4. **Add image's catalog kind.** `POST /base-images` takes a kind (`registry`, `custom`, `byo`);
   the dialog asks only for the ref. The lane picks the kind that means "used as written" (`byo`)
   and confirms it with the server owner.
5. **The empty-Only 400 wording says "Only"** while the control says "Only these". It is a server
   sentence and is drawn verbatim (§5.1).
