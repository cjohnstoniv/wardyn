> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Keep run branches from starting pipelines

- A push or a pull request can start a pipeline, and a pipeline runs with identities the agent does not hold.
- Code an agent wrote can then run with production access if nothing stands between the branch and the pipeline.
- Set the forge first (below), then pick a Wardyn tier. Wardyn adds no new mechanism: every setting here already exists.

## Run branches

- A run pushes only to `refs/heads/wardyn/<run-id>/…` by default ([How pushes work](../AZURE-DEVOPS.md#how-pushes-work)).
- `git_push_any_branch: true` lifts that ([`git_push_any_branch`](../POLICIES.md#git_push_any_branch-the-per-run-opt-out)). A restriction can force it to `false`, and never to `true`.
- The `wardyn/**/*` exclusion on a GitHub ruleset ([`internal/broker/ruleset.go`](../../internal/broker/ruleset.go), `refNamespaceGlob`) lets run pushes through. It does not stop workflows.

## Set these on the forge

Wardyn reads no pipeline trigger and edits no pipeline, policy or service connection unless a run holds the capability for it. These settings are yours to make.

### Azure DevOps

| Setting | What to do | Vendor page |
|---|---|---|
| YAML CI trigger | Add `wardyn/*` under `trigger:` › `branches:` › `exclude:` in every pipeline | [Build Azure Repos Git repositories](https://learn.microsoft.com/en-us/azure/devops/pipelines/repos/azure-repos-git) |
| Trigger overrides | Check whether the pipeline's **Triggers** settings override the YAML trigger | the same page |
| Build validation **Trigger** | Choose **Manual**, or keep production identities out of validation builds | [Set and manage branch policies](https://learn.microsoft.com/en-us/azure/devops/repos/git/branch-policies) |
| **Branch control** check | Add it on production service connections, environments and variable groups, with an allowed-branches list | [Pipeline deployment approvals](https://learn.microsoft.com/en-us/azure/devops/pipelines/process/approvals) |
| **Limit job authorization scope to current project for non-release pipelines** | Turn it on in Organization settings › Pipelines › Settings | [Understand job access tokens](https://learn.microsoft.com/en-us/azure/devops/pipelines/process/access-tokens) |
| **Protect access to repositories in YAML pipelines** | Turn it on | the same page |
| **Prohibit the most recent pusher from approving their own changes** | Turn it on for the target branch | [Set and manage branch policies](https://learn.microsoft.com/en-us/azure/devops/repos/git/branch-policies) |
| Votes after a new push | Under "When new changes are pushed", select **Reset all approval votes (does not reset votes to reject or wait)** or **Reset all code reviewer votes** | the same page |
| **Check for comment resolution** and **Check for linked work items** | Turn both on | the same page |

- With the build validation trigger set to **Automatic**, the vendor page says the policy "queues a new build when you create a new PR or push changes to an existing PR that targets the branch."
- A pull request opened by a run therefore starts validation again on each later push to its branch.
- A work item link satisfies **Check for linked work items**, and `pr_create` can add one.

### GitHub

| Setting | What to do | Vendor page |
|---|---|---|
| `branches-ignore` | Add `branches-ignore: ['wardyn/**']` to the `push` trigger of each workflow. A matching branch does not run the workflow | [Workflow syntax for GitHub Actions](https://docs.github.com/en/actions/writing-workflows/workflow-syntax-for-github-actions) |
| Environments | Keep production secrets in environments with **Required reviewers**; jobs read an environment's secrets only after its rules pass | [Managing environments for deployment](https://docs.github.com/en/actions/managing-workflow-runs-and-deployments/managing-deployments/managing-environments-for-deployment) |
| **Deployment branches** | Choose **Selected branches and tags** and add a rule per allowed branch | the same page |
| **Require a pull request before merging** | Turn it on in a ruleset on the target branch | [Available rules for rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets) |
| **Dismiss stale pull request approvals when new commits are pushed** | Turn it on | the same page |
| **Require approval of the most recent reviewable push** | Turn it on | the same page |

## The Wardyn recipe

Each tier adds to the one before. The push rules are a unit's walls or the run's policy ([`push_rules`](../POLICIES.md#push_rules--pushrulesspec)); the capability part is set as the cell says.

| Tier | Set | Stops | Cost |
|---|---|---|---|
| 1. Keep CI definitions out of reach | The settings below | A brokered push that changes a CI definition; an agent queueing or editing a pipeline or acting on a pull request | None at runtime |
| 2. A person before every push | Tier 1, plus `push_rules.require_review_paths: ["**"]` | Any brokered push, until a second person says yes | Each push waits; an unattended run is refused |
| 3. No pushes | `push_rules.deny_all: true` | Every push | The run cannot hand work back by git |

### Tier 1: keep CI definitions out of reach

| Control | Setting |
|---|---|
| Branch confinement | `git_push_any_branch: false` |
| CI definitions | `push_rules.deny_paths` for `azure-pipelines*.yml`, `.azure-pipelines/**`, `.github/workflows/**`, `.github/actions/**` and the repository's template folders |
| GitHub workflows | `workflows_write` not granted |
| GitHub actions | `actions_execute` and `actions_admin` not granted |
| Azure DevOps pipelines | `build_execute`, `build_admin`, `release_execute`, `release_admin`, `serviceendpoint_admin`, `policy_admin` and `policy_bypass` left out of the provider row's capability ceiling |
| Azure DevOps pull requests | <!-- pr names: added by the lane that splits pr --> `pr_create` in the ceiling but not in the default profile, so opening one is held for the person; `pr_vote` and `pr_complete` left out |
| GitHub pull requests | <!-- pr names: added by the lane that splits pr --> grant `pr_write` in place of `pr`, so `pr_review` is not granted |

- Opening a pull request starts validation, so the hold on `pr_create` applies only when the run's policy names no capability list. A policy that names `pr_create` inside the ceiling makes it standing.
- A capability outside the ceiling is refused with `capability_above_ceiling` ([Capabilities](../AZURE-DEVOPS.md#capabilities)).
- A unit wall's `azure_devops_capabilities` list does not narrow a run on the default profile. It narrows only a run whose own policy names a list, so do not use it to withhold a capability.
- The pipeline capabilities are listed in [Capabilities](../AZURE-DEVOPS.md#capabilities); the GitHub ones in [`github_capabilities`](../POLICIES.md#github_capabilities).
- `**` in a `deny_paths` pattern crosses path segments; `*` and `?` do not.
- A pipeline that runs the branch's own scripts, Makefiles or tests still runs them once a person lets the push or pull request through.

### Tier 2: a person before every push

- A matching push is held as a `push_content` decision. A security admin decides it, and the run's owner cannot approve their own run's push ([Who decides a hold](deciders.md)).
- `hold_seconds` sets the wait: `120` by default, at most `600`. When it runs out, the push is refused.
- A path that matches both `deny_paths` and `require_review_paths` is refused, not held. An unattended run refuses rather than holds.
- A push that removes a file at a review path is not held ([How pushes work](../AZURE-DEVOPS.md#how-pushes-work)).
- This tier also holds a later push to an open pull request's branch, so validation does not re-run on content nobody saw.

### Tier 3: no pushes

- `deny_all` refuses every push to every remote: content, ref updates and deletes.
- A run that holds an `ssh_key`, or a `git_pat` while the git token broker is off, is refused at start, because those pushes skip the broker.

## Where the rule can live

Today each tier's push rules are a unit's walls or a run policy. Its capability part is the provider row's capability ceiling, which a super admin sets.

<!-- per-person and organisation-wide forms: added by the lanes that ship them -->

Each tier's push rules can also be a restriction, so they follow a person into every unit.

<!-- organisation-wide capability cap: added by the lane that ships the boundary's capability lists -->

## What Wardyn guarantees

These hold on brokered lanes. They do not hold for an `ssh_key` grant or a resident PAT, so keep those out with `withheld_grants`.

- Refs stay inside the run's namespace.
- In tier 1, no brokered push changes a listed CI definition.
- In tier 2, no agent-written content reaches the forge without a second person.
- Opening, voting on and completing a pull request is held or refused.
- Wardyn never queues or edits a pipeline, policy or service connection unless a run holds the capability for it.

## What Wardyn cannot guarantee

- What the forge triggers on a push or pull request that a person allowed.
- That a pipeline's scripts, Makefiles, package scripts and tests are safe once a person lets them through. No path list can name them all.
- The pipeline identity's own rights.
- In tier 1, validation re-running when a later push to an open pull request's branch goes through. Tier 2 holds that push.
- A push a person makes outside Wardyn.
- A push that deletes a file at a review path.
