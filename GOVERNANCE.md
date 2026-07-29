# Wardyn Governance

Wardyn is **pre-alpha** and **maintainer-led by a single maintainer** — all
decisions rest with that person today. This is a description of the present state,
not a target.

## Decision making

- **Day-to-day changes** (bug fixes, docs, minor features) use **lazy consensus**: a
  pull request open ≥72 hours with no substantive objection, passing CI, may be
  merged.
- **Significant changes** (API surface, security invariants, architectural
  direction, license or governance) need explicit maintainer approval.
- **Tie-breaking**: the maintainer has final say. There is no second maintainer to
  disagree, so nothing more is specified until there is.

## Maintainers

The list is in [MAINTAINERS.md](MAINTAINERS.md). There is no formal path to
becoming one yet: contribute consistently, show you understand the security
invariants and the threat model, and open an issue (self-nomination is fine at this
scale). See [CONTRIBUTING.md](CONTRIBUTING.md) to get involved.

## Licensing and DCO

Apache-2.0, **inbound = outbound**: by submitting a contribution you license it
under the project's license. Every commit needs a `Signed-off-by` line (DCO 1.1,
enforced in CI) — `git commit -s`. See [CONTRIBUTING.md](CONTRIBUTING.md).

## CNCF Sandbox

A stated **goal**, not a status — Wardyn is not a CNCF project and claims nothing
of the sort. Acceptance would require formalizing this document, a Code of Conduct
enforcement contact, and the due-diligence checklist; none of that is met today.
