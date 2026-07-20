# AI Run Composer — example backend configs

The **AI Run Composer** turns a plain-English task description into a *proposed*
least-privilege sandbox spec (run + inline policy) that Wardyn grades deterministically
and a human reviews before launch. It is **advisory** — the composer backend never
receives the run's credentials.

The composer is **off by default**. You enable it by pointing `WARDYN_COMPOSER_CONFIG`
at one of these files (a path) or at inline JSON. Restart `wardynd` after changing it;
on boot it logs `AI Run Composer enabled (backends=[...] default="...")`.

```bash
# file path
WARDYN_COMPOSER_CONFIG=./examples/composer-configs/fake.json

# …or inline JSON
WARDYN_COMPOSER_CONFIG='{"default":"dev","backends":[{"name":"dev","wire":"fake","model":"demo"}]}'
```

## The configs

| File | Backend | Needs | Use |
|------|---------|-------|-----|
| `fake.json` | deterministic stub | nothing | OOTB demo — exercises the whole compose → review → launch flow with no API key. Proposals are canned (they don't read your prompt). |
| `claude-cli-opus.json` | Claude Code CLI (Opus) | a logged-in `claude` CLI on the host | Real prompt-driven proposals via your Claude **subscription**. Requires running `wardynd` where it can exec `claude` (host mode); rate-limited (CLI ToS). |
| `claude-sandbox-subscription.json` | Claude in a governed sandbox (Opus) | a connected **managed subscription** (`claude setup-token`) | Real prompt-driven proposals via your Claude subscription for **container-mode** wardynd (distroless — no host `claude`). Runs the real `claude` inside a one-shot governed run; the managed token is injected **proxy-side** (never resident). ToS-clean. |
| `anthropic-api.json.example` | Anthropic API (Opus) | an Anthropic API key | Real proposals via the API. Copy to `anthropic-api.json`, then store the key: `wardyn secret set anthropic-api-key`. Not rate-limited; works inside the compose container. |
| `openai-api.json.example` | OpenAI API | an OpenAI API key | Same, for OpenAI. `wardyn secret set openai-api-key`. |

## Config schema

```jsonc
{
  "default": "claude",            // which backend to use unless the UI overrides
  "backends": [
    {
      "name": "claude",           // shown in the UI provider picker
      "wire": "cli",              // "anthropic" | "openai" | "cli" | "sandbox" | "fake"
      "transport": "claude",      // anthropic: api|bedrock · openai: api|azure|compatible · cli: claude|codex · sandbox: (none)
      "model": "claude-opus-4-8", // any model the provider accepts (e.g. "opus" alias also works for the CLI)
      "api_key_secret": "...",    // secret-store name for http key backends (anthropic/openai api)
      "enabled": true,            // cli backends default OFF (subscription ToS) — opt in explicitly
      "timeout_seconds": 120      // cli only
    }
  ]
}
```

API keys are **referenced, never inlined**: `api_key_secret` names a secret in the
at-rest secret store (resolved by `wardynd`, with a `WARDYN_COMPOSER_API_KEY` env
fallback). The config file carries no secret value, so these examples are safe to commit.

## Pluggability

List **multiple** backends to let operators pick a provider/model in the UI (the New Run
wizard's "Describe your task" → Provider dropdown). With a single backend the UI shows it
read-only. Adding a new provider is config-only — the `anthropic` / `openai` / `cli` /
`sandbox` wires are already implemented (`internal/composer/backends/`).
