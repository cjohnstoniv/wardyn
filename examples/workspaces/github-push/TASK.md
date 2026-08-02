# Scenario 5 — GitHub push: brokered git credential chain

## Wardyn run command

This scenario requires a run policy with a github_token grant (demo.json has
one with requires_approval: true).  Use the demo policy or a custom policy
that includes a github_token grant.

    wardyn run \
      --agent claude-code \
      --repo your-org/your-repo \
      --task "In this repository, create a new branch named wardyn/demo-push, add a file named GREETING.md containing the line \"Hello from Wardyn\", commit it with message \"demo: add GREETING.md\", and push the branch to origin. Then attempt to open a pull request titled \"Demo push\" using the GitHub CLI (gh pr create) or the git push --set-upstream command. Report each step's output including any errors."

(The `wardyn/<run-id>/*` branch namespace the broker records is advisory
metadata only — it is NOT enforced yet ([v0.5+ — planned]); a minted token can
push to any branch in the granted repo.)

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
policy, then approve.  The mint succeeds, the push lands in the wardyn/demo-push
branch, and the PR is opened.

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
4. Branch wardyn/demo-push appears in the repository.
5. A PR titled "Demo push" is open.
6. docker exec <sandbox> env | grep -i token is still empty (token was never in env).
