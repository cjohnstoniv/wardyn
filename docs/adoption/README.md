<!-- Copyright 2025 The Wardyn Authors -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Adoption field reports

Reports from operators running Wardyn on real networks — most often a corporate
network, where the failure modes are ones no maintainer's laptop reproduces:
TLS-inspecting proxies, allowlist package mirrors, blocked toolchain fetches.

These are the durable record of *what an adopter actually hit*, kept separately
from the fix so the next person can find the symptom by searching for it. Each
report states the symptom as observed, the verified root cause, what would close
it, and an acceptance test.

**De-identify before committing.** No employer names, internal hostnames, real
proxy URLs, ticket ids, or usernames — the technical content is the point and it
survives redaction intact. Write "a corporate allowlist mirror", not the vendor.

| Report | Status |
|---|---|
| [host-proxy detection is blind on the containerized control plane](host-proxy-detection-blind-on-compose.md) | Fixed in 0.4.1 |
| [`make setup` requires a hand-set `WARDYN_UI_STAGE` behind a pnpm-less mirror](make-setup-requires-ui-stage-on-pnpm-less-mirror.md) | Fixed in 0.4.1 |
| [corporate-network adoption: findings, local fixes, and open gaps](corp-network-onboarding-findings.md) | A1–A4, B4, B5 fixed in 0.4.2; B1–B3 and C1–C2 open |

Reference notes that came out of these reports:

| Note | Covers |
|---|---|
| [a forward proxy bound to loopback](loopback-only-forward-proxy.md) | why sandbox egress can't use it, and `wardyn setup proxy-relay` |
