# github-push workspace

A minimal Go module used to demonstrate the brokered git credential chain.
The agent is asked to create a branch and open a pull request.  Wardyn
intercepts the git credential request via wardyn-git-helper, raises a
credential ApprovalRequest, and — if a GitHub App is configured — mints a
1-hour, repo-scoped, permission-clamped installation token. (Bot-branch
confinement to `wardyn/<run-id>/*` is recorded as advisory metadata but is NOT
enforced yet — [v0.5 — planned]; the token can push to any branch in the
granted repo. See threatmodel/THREAT-MODEL.md asset #4.)

Without a GitHub App configured (the default demo setup), the broker runs
the full chain up to the mint step and then fails closed: the credential
is not issued, the push is rejected, and `credential.mint.fail` is audited.
That fail-closed path is the expected PASS for the stock demo.
