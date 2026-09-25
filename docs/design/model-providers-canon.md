# Model providers list — frozen strings (#536)

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

## Implementation strings

Not in the packet. It draws no failed read, so those follow the console's FETCH_FAILED pattern. It
names five kind labels, so `bedrock_bearer` reuses Bedrock's label, and its what-each-person-provides
line is `MODEL_PROVIDERS.PROVIDES.KEY` (each person adds their own Bedrock API key).

| Key | Renders at | Frozen string |
|---|---|---|
| `MODEL_PROVIDERS.KIND.bedrock_bearer` | second line, only when the name differs | Amazon Bedrock |
| `MODEL_PROVIDERS.FETCH_FAILED_TITLE` | the list's read failed | Couldn't load model providers |
| `MODEL_PROVIDERS.FETCH_FAILED_BODY` | the list's read failed | Something went wrong reaching the server. The providers already saved still apply — this list just can't show them right now. |
