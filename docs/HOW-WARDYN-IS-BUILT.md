# How Wardyn is built

Most of Wardyn's code is written by AI coding agents. We think you should know
that, and know what keeps it honest.

A human maintainer drives the project. They decide what gets built, approve
every design and every user-facing word, make the product and security calls,
test the changes, and direct the agents. No agent merges its own work without
that loop around it.

## How a change gets made today

1. **Plan.** Work starts as an issue. Larger work gets a written plan, and a
   different model checks the plan before anything is built.
2. **Design.** A console change starts as a mock the maintainer approves. The
   approved wording is canonical; code can't invent user-facing text.
3. **Decide.** Product and security trade-offs go to the maintainer. Agents
   raise them; they don't settle them.
4. **Build.** An agent builds against a written spec on its own branch. Every
   behaviour change needs a test that fails before the change and passes after.
5. **Review.** A different, stronger model reviews each change. Anything
   touching sign-in, secrets, credentials, network egress or audit gets a
   dedicated security review. Findings that would ship a defect are fixed and
   reviewed again, until none remain.
6. **Gate.** CI builds and tests everything: unit and Postgres tests, lint,
   static analysis, secret scanning, image vulnerability scans, sign-off checks
   and end-to-end tests. A nightly run adds live Kubernetes walks.
7. **Release.** The whole release is reviewed before it's published, then its
   images are signed with an SBOM and build provenance you can verify
   ([`VERIFY.md`](VERIFY.md)).

## What we're tightening now

- **Every issue gets three reviews before it's scheduled.** Independent agents
  ask: *Applicability* — is this worth doing? *Security* — does it add risk?
  *Accuracy and completeness* — is the plan right, with the requirements and
  validation it needs? An issue that fails one is reworked or closed.
- **Every pull request gets the same three reviews again**, against the issue
  it claims to close, plus code-review agents that read the change itself.
  Rounds continue until no finding would ship a defect.
- **Every release gets two independent reviews**, one by Anthropic's Claude
  Fable and one by OpenAI's GPT-6 Astra, and is verified before it's published.

## If you find a problem

Report security issues privately, as [`SECURITY.md`](../SECURITY.md) describes.
Anything else is welcome as an issue. A review process lowers the odds of a
mistake; it doesn't remove them, and we'd rather hear about one than not.
