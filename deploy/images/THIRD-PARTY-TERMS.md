# Third-party terms in the agent images

Wardyn is Apache-2.0. **Some agent images bundle software that is not.** Read this
before pulling, building, running, or redistributing them.

Wardyn is not affiliated with, endorsed by, or sponsored by Anthropic PBC, OpenAI,
or Amazon Web Services. Product and company names referenced here are the
trademarks of their respective owners and are used only to describe what an image
contains.

## `agent-claude-code` — local build only; contains proprietary software

Published at 0.5.0 and 0.6.0, retired from the release matrix in 0.6.2, and its
GHCR package was removed on 2026-08-30 — no tag of it is pullable today. The
Dockerfile remains a local build recipe (`make agent-images`), so everything
below binds anyone who builds and runs the image themselves.

| | |
|---|---|
| Component | `@anthropic-ai/claude-code` and `@anthropic-ai/claude-code-linux-x64` |
| Version in `0.6.1` | 2.1.231 |
| Declared licence | `SEE LICENSE IN README.md` / `SEE LICENSE IN LICENSE.md` — **not an SPDX licence** |
| Actual terms | © Anthropic PBC, all rights reserved. Use subject to Anthropic's Commercial Terms of Service. |
| Terms | https://code.claude.com/docs/en/legal-and-compliance |

This is **not open-source software** and Wardyn grants you no rights to it. Neither
of the referenced licence files ships inside the image, so the terms cannot be read
from the artifact — follow the link above.

Anthropic permits preinstalling Claude Code in agent infrastructure, but
conditionally. Conditions that fall on **you as the operator**, not on Wardyn:

- You must agree to Anthropic's Commercial Terms of Service.
- The Claude Code binary must not be modified, and its built-in authentication
  methods must not be removed, disabled, or restricted.
- **Each end user must authenticate with their own credential** — their own API
  key, their own Claude subscription, or their own third-party inference provider.
  You may not pay for, resell, or intermediate Claude usage on your users' behalf.
- You may state that your product has Claude Code preinstalled, but may not use
  Anthropic's names or logos as part of your own product identity or in a way that
  suggests endorsement or partnership.

> **Operator warning — shared subscription credentials.** Wardyn's subscription
> injection path resolves a single operator credential at the proxy. In a
> multi-user deployment that means one person's Claude subscription serving other
> authenticated users' runs, which is precisely what the third condition above
> prohibits. If you run Wardyn multi-user against a subscription, use per-user
> credentials or API-key mode. `WARDYN_SUBSCRIPTION_INJECT: "off"` is the compose
> default for this reason. Compliance with your harness vendor's terms is the
> operator's responsibility; Wardyn cannot discharge it for you.

## `agent-codex-cli` — open source

| | |
|---|---|
| Component | `@openai/codex` |
| Version in `0.6.1` | 0.149.1 |
| Licence | **Apache-2.0**, with an upstream NOTICE |

Genuinely open source. No additional terms are known to attach. Listed here only
so the distinction from the image above is explicit rather than assumed.

## `agent-aws-sso`

Bundles the AWS CLI v2 (Apache-2.0), installed from AWS's distribution and
GPG-signature-verified at build time. Its bundled Python runtime is not visible to
container SBOM tooling; treat the AWS CLI as an opaque vendored component when
inventorying this image.

## All agent images

Every agent image is built on a Debian or Alpine base and therefore conveys GPL-
and LGPL-licensed packages, plus an apt-installed `asciinema` (GPL-3.0). See
[`THIRD-PARTY-GPL.md`](THIRD-PARTY-GPL.md) for the corresponding-source offer.

## If you would rather not receive any of this

Build the agent image yourself from the Dockerfile in this directory. `make
agent-images` does it locally, and the bring-your-own-image path is supported. You
then install the vendor CLI under your own agreement with that vendor, and Wardyn
conveys nothing to you but its own Apache-2.0 code.
