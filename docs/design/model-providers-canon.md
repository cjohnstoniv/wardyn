# Model providers list and editor — frozen strings (#536, #537)

The approved copy from mock packet MP-A ("Model Providers List"), byte for byte. Code carries these
strings (`ui/src/app/lib/model-providers-copy.ts`); this table is where a reviewer checks them, and
`model-providers-copy.test.ts` parses it back out. A `{name}` in a cell is the placeholder the key
takes. The packet supersedes the design record's §5.1 chips: the admin's own connection
(`MODEL_PROVIDERS.YOU_*`) is retired from this page and lives in the person's own account.

## Owner decisions (packet MP-A)

| Id | Decision |
|---|---|
| QA-1 | The harnesses on a row are text: "Used by Claude Code, Codex CLI". |
| QA-2′ | The count includes expired sign-ins; the word stays "connected", never "working". |
| QA-3 | "Default for" shows on the provider row as well, read-only; it is set on the Agents tab. |
| QA-4 | The empty state reads as drawn in A1. |
| QA-5 | The name defaults to the kind label, and a row whose name equals its kind label shows it once. |

## Frozen strings

| Key | Renders at | Frozen string |
|---|---|---|
| `MODEL_PROVIDERS.TITLE` | A1, A2, A9 | Model providers |
| `MODEL_LEDE` | A1, A2, A9 (reused as the Model provider card's `S.MODEL_LEDE`) | Agent runs need one. Governed commands don't. |
| `MODEL_PROVIDERS.ADD_CTA` | A1, A2, A9 | Add model provider |
| `MODEL_PROVIDERS.EMPTY_TITLE` | A1 | No model providers yet |
| `MODEL_PROVIDERS.EMPTY_BODY` | A1 | Agent runs need one. Add the kinds your organisation uses — each person connects their own. |
| `MODEL_PROVIDERS.KIND.anthropic_subscription` | second line, only when the name differs | Claude subscription |
| `MODEL_PROVIDERS.KIND.bedrock_sso` | second line, only when the name differs | Amazon Bedrock |
| `MODEL_PROVIDERS.KIND.anthropic_api_key` | second line, only when the name differs | Anthropic API key |
| `MODEL_PROVIDERS.KIND.openai_api_key` | second line, only when the name differs | OpenAI API key |
| `MODEL_PROVIDERS.KIND.custom_endpoint` | second line, only when the name differs | Your own endpoint |
| `MODEL_PROVIDERS.PROVIDES.SSO` | A2, A5, A9 | Each person signs in with one click |
| `MODEL_PROVIDERS.PROVIDES.TOKEN` | A3, A4 | Each person adds their own token |
| `MODEL_PROVIDERS.PROVIDES.KEY` | A3, A5, A7, A8 | Each person adds their own key |
| `PROVIDER_EDITOR.PROVIDES_CLAUDE` | A3 subscription row (packet B's line) | Each person signs in with their own Claude subscription. |
| `MODEL_PROVIDERS.USED_BY(harness)` | A2–A9 | Used by {harness} |
| `MODEL_PROVIDERS.USED_BY(harness, harness)` | A4 | Used by Claude Code, Codex CLI |
| `MODEL_PROVIDERS.CHIP_DEFAULT_FOR(harness)` | A3–A5, A7 | Default for {harness} |
| `MODEL_PROVIDERS.CHIP_DEFAULT_FOR_BOTH` | A4 variant | Default for Claude Code and Codex CLI |
| `MODEL_PROVIDERS.CONNECTED(n)` | every row | Connected by {n} people |
| `MODEL_PROVIDERS.CONNECTED(1)` | every row | Connected by 1 person |
| `MODEL_PROVIDERS.CONNECTED(0)` | every row | No one has connected yet |
| `MODEL_PROVIDERS.CHIP_OFF` | A7 | Off |
| `MODEL_PROVIDERS.OFF_LINE` | A7 | Runs can't choose it. |
| `MODEL_PROVIDERS.OFF_STILL_DEFAULT(harness)` | A7 | Off — it's still the default for {harness}, so those runs are refused until you choose another default. |
| `MODEL_PROVIDERS.UNUSED` | A8 | Not used by any agent — tick one under Use with. |
| `MODEL_PROVIDERS.HARNESS_UNSERVED(harness)` | A9 | {harness} is turned on, but no model provider is set up for it. Its runs launch without model access. |

## Frozen strings — the provider editor (#537)

The #537 packet's rows: `docs/design/model-providers-mock/canon.html` Table 1 (branch
`design/551-providers-mock-packet`, owner-approved 2026-09-25), under the same ids. Its
`MODEL_PROVIDERS.ADD_CTA` and kind-label rows are the ones above. The two agent reasons it reuses,
`INTEGRATIONS.X_KEY_CODEX` and `INTEGRATIONS.X_OPENAI_CLAUDE`, are `lib/integrations.ts`'s own.

| Key | Renders at | Frozen string |
|---|---|---|
| `PROVIDER_EDITOR.KIND_TITLE` | kind step | What kind of model provider? |
| `PROVIDER_EDITOR.PROVIDES_KEY` | E1, E5 | Each person adds their own key. |
| `PROVIDER_EDITOR.PROVIDES_TOKEN` | E2, edit | Each person adds their own token. You set how it is sent. |
| `PROVIDER_EDITOR.NAME` | every kind | Name |
| `PROVIDER_EDITOR.NAME_HINT` | every kind | What people see when they choose it. |
| `PROVIDER_EDITOR.ROUTE_THROUGH` | key kinds | Route through a gateway (optional) |
| `PROVIDER_EDITOR.ROUTE_THROUGH_HINT(host)` | key kinds; `api.anthropic.com` or `api.openai.com` | Send requests to your gateway instead of {host}. Each person's key goes with them, and they are told where it goes. |
| `PROVIDER_EDITOR.BASE_URL` | endpoint kind | Base URL |
| `PROVIDER_EDITOR.BASE_URL_HINT` | endpoint kind | https only. Wardyn sends this provider's requests here. |
| `PROVIDER_EDITOR.AUTH_HEADER` | endpoint kind | Auth header |
| `PROVIDER_EDITOR.VALUE_FORMAT` | endpoint kind | Value format |
| `PROVIDER_EDITOR.USE_WITH` | every kind | Use with |
| `PROVIDER_EDITOR.MODEL` | under a ticked agent | Model |
| `PROVIDER_EDITOR.MODEL_HINT` | under a ticked agent | Leave empty for the agent's own default. |
| `PROVIDER_EDITOR.PATH` | endpoint kind, under a ticked agent | Path |
| `PROVIDER_EDITOR.PATH_HINT_CLAUDE` | E2 | Where your endpoint serves the Anthropic Messages API for Claude Code, e.g. /anthropic. |
| `PROVIDER_EDITOR.PATH_HINT_CODEX` | E2 | Where your endpoint serves the OpenAI Responses API for Codex CLI, e.g. /v1. |
| `PROVIDER_EDITOR.CANCEL` | every dialog | Cancel |
| `PROVIDER_EDITOR.SAVE` | editor; E9 confirm | Save |
| `PROVIDER_EDITOR.REMOVE` | edit footer; E7, E8 confirm | Remove |
| `PROVIDER_EDITOR.SAVED_TOAST` | toast after Save | Provider saved. |
| `PROVIDERS.SAVE_REFUSED_TITLE_ONE` | E6, over the server's 400 | This provider can't be saved as written |
| `PROVIDER_EDITOR.DELETE_TITLE(name)` | E7, E8 | Remove {name}? |
| `PROVIDER_EDITOR.DELETE_BODY` | E7, endpoint kind | Runs that chose it are refused until they choose another. Everyone's tokens for it are deleted. |
| `PROVIDER_EDITOR.DELETE_BODY_KEY` | E7, key kinds | Runs that chose it are refused until they choose another. Everyone's keys for it are deleted. |
| `PROVIDER_EDITOR.DELETE_BLOCKED(name, harness)` | E8 | {name} is the default for {harness} — choose another default first. |
| `PROVIDER_EDITOR.ADDRESS_TITLE(name)` | E9 | Change where {name} sends requests? |
| `PROVIDER_EDITOR.ADDRESS_BODY(n)` | E9, endpoint kind, two or more people | The tokens {n} people added were given for the old address, so they are deleted. Everyone who connected adds theirs again. |
| `PROVIDER_EDITOR.ADDRESS_BODY_ONE` | E9, endpoint kind, one person | The token 1 person added was given for the old address, so it is deleted. They add it again. |
| `PROVIDER_EDITOR.ADDRESS_BODY_KEY(n)` | E9, key kinds, two or more people | The keys {n} people added were given for the old address, so they are deleted. Everyone who connected adds theirs again. |
| `PROVIDER_EDITOR.ADDRESS_BODY_KEY_ONE` | E9, key kinds, one person | The key 1 person added was given for the old address, so it is deleted. They add it again. |

## Implementation strings

Not in the packet. It draws no failed read, so those follow the console's FETCH_FAILED pattern. It
names five kind labels, so `bedrock_bearer` reuses Bedrock's label, and its what-each-person-provides
line is `MODEL_PROVIDERS.PROVIDES.KEY` (each person adds their own Bedrock API key).

| Key | Renders at | Frozen string |
|---|---|---|
| `MODEL_PROVIDERS.KIND.bedrock_bearer` | second line, only when the name differs | Amazon Bedrock |
| `MODEL_PROVIDERS.FETCH_FAILED_TITLE` | the list's read failed | Couldn't load model providers |
| `MODEL_PROVIDERS.FETCH_FAILED_BODY` | the list's read failed | Something went wrong reaching the server. The providers already saved still apply — this list just can't show them right now. |
