# #542 rail-gap canon

Proposed additions to `ui/src/app/components/wardyn/copy/new-run-rail.ts`'s `RAIL_PROVIDER`
(the object PR #1036 added). These are final — an approved string here becomes the app string
byte for byte, same rule as every canon file in `docs/design/`. Every string below also
appears verbatim in `rail.html` — checked by grep, not by an existing repo script (see the
report for the exit code).

`{placeholder}` is filled by the caller; the text inside the braces in an example is the fixture
used to draw that string in `rail.html`.

## R3 — selected, not connected (the three missing kinds)

`ProviderNotConnectedLine` (`new-run-rail.tsx`) already has a branch for `bedrock_sso`
(`RAIL_PROVIDER.NOT_SIGNED_IN`) and `custom_endpoint` (`RAIL_PROVIDER.NO_TOKEN`). These two
cover the rest of `providerWhatWord`'s vocabulary — "key" (`anthropic_api_key`,
`openai_api_key`) and the sign-in kind that isn't Bedrock (`anthropic_subscription`).

| Id | String | Example | Why |
|---|---|---|---|
| `RAIL_PROVIDER.NO_KEY(name)` | `You haven't added your key for {name}.` | `You haven't added your key for Direct Anthropic.` / `You haven't added your key for Direct OpenAI.` | One sentence for both key kinds (D2) — mirrors `NO_TOKEN`'s shape exactly, swapping "token" for "key", the same word `providerWhatWord`/`RAIL_PROVIDER.OPTION` already use for both. |
| `RAIL_PROVIDER.NOT_SIGNED_IN_CLAUDE(name)` | `You're not signed in to Claude for {name}.` | `You're not signed in to Claude for Personal Claude.` | Mirrors `NOT_SIGNED_IN`'s shape, swapping "AWS" for "Claude" — the mechanism word `RAIL_CREDENTIAL.SANDBOX_SUBSCRIPTION` already uses. Keeps "for {name}" (D3): this string sits beside `NOT_SIGNED_IN`/`NO_TOKEN` in the same object, both of which name the provider so two candidates of the same kind read differently. The wording matches the server's own refusal vocabulary already shipped in `docs/design/model-providers-mock/canon.html`'s `LLM_PROVIDER_REFUSAL` states ("you are not signed in to Claude for it"), contracted to `RAIL_PROVIDER`'s existing tone. |

Both reuse buttons already canon in `ui/src/app/components/wardyn/copy/door.ts`'s
`CONNECTIONS`: `ADD_KEY` ("Add your key") for the two key kinds, `SIGN_IN_CLAUDE` ("Sign in to Claude") for the subscription kind — no new button string needed.

## R5b — granted none

Zero candidates for the picked agent: no provider serves this person for this harness at all.
No select renders; Launch is refused rather than let the run start with no model access.

| Id | String | Example | Why |
|---|---|---|---|
| `RAIL_PROVIDER.NOT_GRANTED(harness)` | `You haven't been granted a model provider for {harness} — ask your admin.` | `You haven't been granted a model provider for Claude Code — ask your admin.` | Verbatim from packet C's frozen-strings table (§5.6) — the owner ruled R5 out of PR #1036's build, not out of canon. "Your admin" stays generic (D1): no admin-identity field exists on `SetupModelProvider` to name one. |

This is the packet's proposed re-approval of a string packet C already drafted; nothing here
changes it.

## R5c — the default is turned off

The admin's default for this harness is a disabled provider. It is never offered in the select
(the same `!p.disabled` filter `providerCandidates` already applies); only an explicit pick
among what remains gets past it, even when exactly one candidate remains.

| Id | String | Example | Why |
|---|---|---|---|
| `RAIL_PROVIDER.DEFAULT_OFF(name, harness)` | `{name}, the default for {harness}, is turned off. Choose another model provider to launch.` | `Corp gateway, the default for Claude Code, is turned off. Choose another model provider to launch.` | Verbatim from packet C (§5.6). Names the disabled default so the person knows it isn't a bug that nothing preselected, and the second sentence is the same instruction `RAIL_PROVIDER.LAUNCH_HINT` gives R6 — no separate `LAUNCH_HINT` line needed here, since this sentence already ends on it. |
| `RAIL_PROVIDER.DEFAULT_OFF_ONLY(name, harness)` | `{name}, the default for {harness}, is turned off. Ask your admin.` | `Corp gateway, the default for Claude Code, is turned off. Ask your admin.` | Verbatim from packet C (§5.6). Same first sentence as `DEFAULT_OFF`; the second sentence changes because there is nothing left to choose — the person is blocked, not just prompted. |

Both reuse `RAIL_PROVIDER.OPTION` and `RAIL_PROVIDER.PLACEHOLDER` (already canon) for the
select itself when `DEFAULT_OFF`'s other-candidates case renders a picker.
