# Licensing

The one page to read if you are evaluating Wardyn for use inside an organisation.

## The short answer

Wardyn is licensed under the **Apache License, Version 2.0**. It is free for anyone
to use, for any purpose, including commercial purposes, at no charge.

That means, explicitly:

- **Free for individuals and free for enterprises**, at any scale. There are no
  seat limits, no node limits, no run limits, and no usage tiers.
- **No fee, ever, for the software.** There is no paid edition, no `enterprise/`
  directory, no open-core split, no feature held back behind a licence key. Every
  control the project documents is in this repository and runs on your
  infrastructure, or it does not run.
- **No registration, no activation, no phone-home.** The product contains no
  licence-key, entitlement, seat-limit, or billing logic, and no analytics or
  telemetry SDK of any kind. Nothing in Wardyn reports your usage to anyone.
- **You may run it internally, modify it, embed it in your own product, and offer
  it as a service**, without asking us and without owing us anything. Apache-2.0
  §2 grants that directly.
- **You may redistribute it**, provided you carry the licence and notices with it —
  see [Obligations](#obligations-if-you-redistribute-wardyn) below.

## Why this cannot quietly change later

The usual risk with a free open-source project is that its owner relicenses it —
to BUSL, SSPL, or a commercial licence — once adoption makes that lucrative. For
most projects that is possible because a Contributor Licence Agreement assigns
copyright to a single party who can then do as they like with it.

**Wardyn has no CLA.** Contributions come in under the Developer Certificate of
Origin, and each contributor keeps copyright in their own work. Inbound equals
outbound: Apache-2.0 in, Apache-2.0 out. The practical consequence is that **no
single party — including the maintainer — can relicense the project's accumulated
work**. That is a structural guarantee, not a promise, and it is worth more to an
adopter than any statement of intent.

An Apache-2.0 grant, once made, is also irrevocable for the code it covers. A
future version could in principle be published under other terms, but every
version released to date stays Apache-2.0 forever, and anyone may fork from it.

## What you get, and what it contains

Wardyn is distributed through four channels:

| channel | what it is |
|---|---|
| **Source** | this git repository |
| **Container images** | `ghcr.io/cjohnstoniv/{wardynd,wardyn-proxy,agent-base,agent-codex-cli,agent-aws-sso}` |
| **CLI binaries** | `wardyn-{linux,darwin}-{amd64,arm64}`, attached to each GitHub release |
| **Helm chart** | pushed to `oci://ghcr.io/cjohnstoniv/charts` on release; also installable straight from this repo |

Three more agent Dockerfiles (`vscode`, `novnc`, `full`) are **local build
recipes only**: `make agent-images` builds them on your own machine, no registry
publishes them, and a release gate keeps them out of the publish matrix.

**Wardyn's own code and all of its dependencies are permissively licensed.** Every
Go module compiled into the shipped binaries and every npm package bundled into
the console is Apache-2.0, MIT, BSD, ISC or OFL-1.1. There is no copyleft, no
source-available licence, and no unlicensed dependency anywhere in the product.
This is enforced on every build, not merely asserted: `make licenses` and
`make npm-license` check every dependency against a single allowlist at
`licenses/ALLOWED-LICENSES.txt` and fail the build on anything else, including a
dependency that declares no licence at all.

**The container images additionally contain third-party software that Wardyn does
not license to you**, because an image is an operating system, not just our
binary:

- Every image carries base-image packages under GPL and LGPL (Debian's `bash`,
  `coreutils`, `dpkg`, `apt`; Alpine's `busybox` and `apk-tools`). This is
  ordinary aggregation and is how every container image on earth works, but the
  corresponding source offer is real and is in
  [`deploy/images/THIRD-PARTY-GPL.md`](deploy/images/THIRD-PARTY-GPL.md).
- The agent images apt-install `asciinema` (GPL-3.0), executed as a subprocess and
  never linked. Covered by the same source offer.
- **No published image bundles a proprietary AI coding CLI.** The retired
  `agent-claude-code` image did; it left the release matrix in 0.6.2 and its
  GHCR package was removed on 2026-08-30. Its Dockerfile remains a local build
  recipe, and [`deploy/images/THIRD-PARTY-TERMS.md`](deploy/images/THIRD-PARTY-TERMS.md)
  states the vendor terms that bind anyone who builds and runs it themselves.
  The two product images (`wardynd`, `wardyn-proxy`) contain no such component.

You do not have to take any of this on trust: every published image is
cosign-signed and carries an attested CycloneDX SBOM and build provenance.
[`docs/VERIFY.md`](docs/VERIFY.md) is the copy-pasteable procedure.

The complete component inventory is in
[`THIRD-PARTY-NOTICES.md`](THIRD-PARTY-NOTICES.md), with verbatim licence texts in
`licenses/texts/`. Both are generated, CI-verified against drift, and shipped
inside every image at `/usr/share/doc/wardyn/`.

## Components invoked as separate processes

Some features run third-party tools as separate processes or containers rather
than linking them into Wardyn's binaries. Each is obtained unmodified under its
own licence and is not part of the Wardyn distribution:

- **envbuilder** (`ghcr.io/coder/envbuilder`, Apache-2.0) — workspace image
  builds run the unmodified upstream image as its own container, pinned by tag
  and digest in `internal/envbuild/builder.go` and overridable for air-gapped
  or newer-pin deployments. Wardyn drives it through its documented environment
  interface and links none of its code.
- **asciinema** (GPL-3.0) — terminal recording inside the agent images,
  executed as a subprocess and never linked; its corresponding-source offer is
  in [`deploy/images/THIRD-PARTY-GPL.md`](deploy/images/THIRD-PARTY-GPL.md).

## Obligations if you redistribute Wardyn

Running Wardyn internally imposes nothing on you. If you redistribute it — ship it
to customers, publish a fork, or mirror the images into a registry others pull
from — Apache-2.0 §4 asks four things:

1. Give recipients a copy of the licence.
2. Mark files you changed as changed.
3. Keep the copyright, patent, trademark and attribution notices from the source.
4. Carry the `NOTICE` file's contents.

Copying `LICENSE`, `NOTICE` and `THIRD-PARTY-NOTICES.md` alongside whatever you
ship satisfies all four. If you redistribute the *images*, you also inherit the
GPL source-offer obligation for their base packages and the vendor terms for any
bundled CLI; both files linked above tell you exactly what those cover.

## Patents

Apache-2.0 §3 grants you a patent licence from every contributor, covering their
contributions, for as long as you do not initiate patent litigation over the
software. Permissive licences without a patent grant — MIT, BSD — give you no such
protection. The inbound side is Apache-2.0 §5: a contribution submitted for
inclusion is under the same terms, which is what conveys the patent grant in a
DCO-only project with no CLA.

## Trademarks

Apache-2.0 §6 grants **no** trademark rights. What you may and may not call things
is a separate question, answered in [`TRADEMARKS.md`](TRADEMARKS.md). The short
version: you may say what your software is built on, unmodified redistribution may
keep the name, and modified builds must be renamed.

## Warranty, indemnity, support

Apache-2.0 §§7–8 disclaim warranty and liability, and this project makes no
exception to them. There is no commercial entity behind Wardyn, so there is no
warranty, no indemnity, no support contract and no SLA available to purchase.
Security reports are handled per [`SECURITY.md`](SECURITY.md).

## Export control

Wardyn implements no cryptography of its own but does use standard published
algorithms via third-party libraries. See [`docs/EXPORT.md`](docs/EXPORT.md) for
the self-classification and the crypto inventory.

## Data protection

Wardyn collects nothing and transmits nothing to the project or its maintainers.
It has no backend service, no update check, and no usage reporting. There is
consequently no data-processing agreement to sign, because there is no processing
to agree about.

## Disclosed risks

Stated plainly rather than buried, because an evaluator will find them anyway:

- **Status is pre-alpha.** Interfaces are not stable and `README.md` says not to
  run production workloads. That is an honest maturity signal, not a licensing
  restriction — the licence grant above is unconditional regardless.
- **The name is provisional.** Trademark clearance for "Wardyn" is pending, so the
  name and the `github.com/cjohnstoniv/wardyn` module path may change before 1.0.
  If it does, a Go import path change would affect consumers; see `TRADEMARKS.md`.
- **Single maintainer.** Bus factor is one. `GOVERNANCE.md` is candid about it.
- **History was squashed before public release**, so per-file provenance predating
  the initial commit cannot be reconstructed from git. See
  [`PROVENANCE.md`](PROVENANCE.md).

## Questions

Anything not answered here: open a discussion on the repository. For matters your
counsel needs in writing, say so in the issue and it will be addressed in this
file, so the answer is public and durable rather than living in someone's inbox.
