# Wardyn Governance

Wardyn is **pre-alpha** software under active development. This document describes
how the project is governed today and where it is headed.

## Current status

Wardyn is currently **maintainer-led by a single maintainer**. All significant
decisions rest with that maintainer while the project is pre-alpha. This is an
honest description of the present state, not a long-term goal.

The project targets **CNCF Sandbox** as its governance model destination. CNCF
Sandbox acceptance is an **aspiration and a goal** — Wardyn is not currently a
CNCF project and makes no claim to that status today.

## Decision making

**Day-to-day changes** (bug fixes, documentation, minor features) use **lazy
consensus**: a pull request that has been open for at least 72 hours with no
substantive objection and that passes CI may be merged by the maintainer.

**Significant changes** (API surface, security invariants, architectural
direction, license or governance changes) require explicit maintainer approval
and, once there are multiple maintainers, a majority of active maintainers.

**Tie-breaking**: while there is a single maintainer, that person has final say.
When there are multiple maintainers, the tie-break process will be documented
here.

## Maintainers

The current maintainer list is in [MAINTAINERS.md](MAINTAINERS.md).

Contributions and maintainer candidates are welcome. See
[CONTRIBUTING.md](CONTRIBUTING.md) for how to get involved.

### Becoming a maintainer

There is no formal process yet. In practice:

1. Contribute consistently over a period of time (code, review, docs, or
   operational work).
2. Demonstrate familiarity with the security invariants and the threat model.
3. Be nominated by an existing maintainer (currently, self-nomination with a
   GitHub issue is also accepted given the project scale).
4. Receive a lazy-consensus non-objection from all existing maintainers over
   seven days.

When Wardyn applies for CNCF Sandbox, the governance process will be aligned with
CNCF requirements, which may require formalizing the above.

## Licensing and DCO

Wardyn is licensed under the **Apache License 2.0**. Contributions follow an
**inbound = outbound** model: by submitting a contribution you license it under
Apache-2.0, the same license as the project.

All commits must carry a `Signed-off-by` line (enforced in CI) as required by
the **Developer Certificate of Origin (DCO) 1.1**. See [CONTRIBUTING.md](CONTRIBUTING.md)
for the full DCO text and sign-off instructions.

## CNCF Sandbox goal

Achieving CNCF Sandbox status is a stated project goal. Progress toward that
goal is tracked in GitHub issues and milestones. Meeting CNCF Sandbox requirements will
include formalizing this governance document, establishing a Code of Conduct
enforcement contact, and satisfying CNCF's due-diligence checklist. None of
those requirements are currently met in full; this document and the accompanying
community-health files are a step in that direction.
