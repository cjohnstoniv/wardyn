# Trademark and naming policy

Apache-2.0 §6 grants no trademark rights. The licence tells you what you may do
with the *code*; this file tells you what you may do with the *name*. Without it
the safe assumption a legal reviewer must make is "not permitted", which would
block the ordinary internal rebuild every large organisation does.

## Current status, stated plainly

"Wardyn" is a working name. Trademark clearance — USPTO full-text search plus
organisation, domain and package-handle availability — is still pending, and the
name may change before 1.0. No registered mark exists today. This policy describes
how the project asks you to use the name, and it will be restated in binding terms
if and when a mark is registered.

## What you may do without asking

- **Say what your software is built on.** "Built on Wardyn", "Wardyn-compatible",
  "runs Wardyn" — accurate, descriptive references to the project are fine, and
  are nominative fair use regardless of this policy.
- **Redistribute Wardyn unmodified under its own name.** Mirroring the images into
  your internal registry, vendoring the source, or shipping the unmodified
  artifacts to your own users: keep the name.
- **Rebuild from unmodified source and keep the name.** Building your own images
  from this repository, for internal use, without functional changes, is still
  Wardyn. Enterprises that rebuild everything from source rather than pulling
  public images do not lose the name by doing so.
- **Use the name in documentation, talks, articles and comparisons**, including
  critical ones.

## What requires renaming

- **Distribute a modified build under the name "Wardyn".** If you change what the
  software does and pass it on, call it something else. You may still say it is
  derived from Wardyn.
- **Name your own product, company, or service "Wardyn"**, or something close
  enough to be confused with it.
- **Imply endorsement, affiliation, certification, or partnership** that does not
  exist.

## Forks

Fork freely — that is what the licence is for. A fork that diverges functionally
should carry its own name, and should say it began as a fork of Wardyn. Keeping
the upstream name on a diverged fork is the one thing that genuinely confuses
users, which is the only interest this policy protects.

## Logos

There is no logo yet. When there is, it will be licensed separately from the code,
as is usual, and this section will say so.

## If the name changes

If clearance fails and the project is renamed, the Go module path
`github.com/cjohnstoniv/wardyn` changes with it, which would break `go.mod` for
consumers. The project's commitment: if that happens before 1.0, the old import
path will remain resolvable for at least one minor release cycle after the rename,
with the deprecation stated in `CHANGELOG.md` and `README.md`. After 1.0 the module
path is stable.

## Governance note

`GOVERNANCE.md` names CNCF Sandbox as a goal. Donating a project to the Linux
Foundation normally includes assigning the trademark to it. This policy is written
to be compatible with that outcome rather than to preclude it.

## Contact

Questions about name usage: open a discussion on the repository.
