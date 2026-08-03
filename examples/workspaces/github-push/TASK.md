# Scenario 5 — GitHub push: brokered git credential chain

## Wardyn run command

This scenario requires a run policy with a github_token grant (demo.json has
one with requires_approval: true).  Use the demo policy or a custom policy
that includes a github_token grant.

    wardyn run \
      --agent claude-code \
      --repo your-org/your-repo \
      --task "In this repository, you are already on the branch wardyn/\$WARDYN_RUN_ID/work — stay on it. Add a file named GREETING.md containing the line \"Hello from Wardyn\", commit it with message \"demo: add GREETING.md\", and push the branch to origin. Then attempt to open a pull request titled \"Demo push\" with the GitHub CLI (gh pr create) and report the exact error verbatim — it is expected to fail. Report each step's output including any errors."

(Push branch-namespace confinement is **ON by default**: the broker refuses any
ref outside `refs/heads/wardyn/<run-id>/`, before the token is minted. `agent-run`
already checked the clone out onto `wardyn/$WARDYN_RUN_ID/work`, so the task above
just stays there. To watch the refusal instead, tell the agent to create and push
`wardyn/demo-push` — that is outside the namespace and 403s with a
`brokered:git:branch-ns` deny row. `WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS=false` on
the proxy turns enforcement off.)

(The `gh pr create` half is expected to FAIL, and that is the point: the git
broker manages `api.github.com` too, so a brokered run's egress denies it —
there is no route from the sandbox to the GitHub API. The agent pushes the
branch through the broker; a human, or a CI job outside the sandbox, opens the
PR from it. Demonstrating that refusal is a stronger property than the PR.)

## What to watch

- UI > Approvals tab: a credential approval request (kind=credential) appears
  when wardyn-git-helper calls the mint route.  The scope shows the requested
  GitHub permissions.  (Raising it is not itself an audit event — the decision
  and the mint are.)
- UI > Audit tab: events in order --
    run.exec        success           (agent started)
    approval.decide outcome=approved  (after you approve)
    credential.mint outcome=success   (token minted, repo-scoped)
      -- OR --
    credential.mint outcome=failure   (no GitHub App configured -- expected for demo)

### Demo (no GitHub App): approve path still shows fail-closed

    wardyn approve <approval-id>

After approval, wardynd attempts the mint but finds no GitHub App credentials.
A `credential.mint` event with `outcome=failure` is emitted and the push fails.  This is
the correct fail-closed behavior documented in docs/TRY-IT.md.

### Real GitHub App path

demo.json's github_token grant is READ-ONLY (`permissions: {contents: read}`) —
it deliberately ships least-privilege to prove the fail-closed path above. A
real branch push + PR needs a WRITE-scoped grant, so even with a GitHub App
configured the mint from demo.json yields a read-only token and the push/PR
still fail. To exercise the full push+PR outcome, run with a policy whose
github_token grant requests `contents: write` + `pull_requests: write` —
`examples/policies/composer-dev.json` ships exactly that shape.

Configure the App as described in docs/TRY-IT.md (wardyn secret set github-app-id,
wardyn secret set github-app-key), restart wardynd, run with the write-scoped
policy, then approve.  The mint succeeds and the push lands in the
`wardyn/<run-id>/work` branch.  The PR does NOT open from inside the sandbox —
`api.github.com` is broker-managed and denied for a brokered run — so open it
from the pushed branch yourself.

## PASS criteria

Stock demo (no GitHub App configured):
1. A PENDING kind=credential approval appears in the Approvals tab.
2. After approving: audit contains approval.decide outcome=approved.
3. Audit contains credential.mint with outcome=failure (broker could not mint
   without a GitHub App).
4. The agent reports a push error (authentication failed or similar).
5. No GitHub token appears in docker exec env output (verify: docker exec <sandbox> env | grep -i token is empty).

With GitHub App configured AND a write-scoped policy (contents:write +
pull_requests:write, e.g. examples/policies/composer-dev.json — NOT read-only
demo.json):
1-2. Same as above.
3. Audit contains credential.mint success with a short-lived JTI.
4. Branch `wardyn/<run-id>/work` appears in the repository — the run's own
   namespace, NOT a name from the task text. (`agent-run` created it at clone
   time; a push to anything outside `wardyn/<run-id>/*` 403s before the mint,
   with a `brokered:git:branch-ns` deny row in the decision log.)
5. `gh pr create` FAILED and the agent reported the error — `api.github.com` is
   broker-managed, so a brokered run has no route to the GitHub API. No PR is
   opened from inside the sandbox; open it yourself from the pushed branch.
6. docker exec <sandbox> env | grep -i token is still empty (token was never in env).
7. The run's effective policy (audit event `run.policy.effective`) lists NO
   github host under allowed_domains, and lists them under denied_domains — the
   brokered route is the only route.
