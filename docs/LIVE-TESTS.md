# Live-local tests

A small set of tests that run against real services: an Entra tenant, an Azure
DevOps organisation, and an AWS account with IAM Identity Center and Bedrock.
They exist to prove what the hermetic fakes can't. They are opt-in and local
only.

## Suites

- They never run in CI or in `make ci`.
  - The Go suites build only with `-tags live`.
  - The browser suites have their own Playwright config, which no other config or script picks up.
- Every suite skips unless its own gate variable is `1`.
  - The skip message names the variables it needs and never prints a value.
  - Once the gate is `1`, the Go suites fail, not skip, on a missing variable.
- Only code is committed.
  - There are no recorded responses, fixtures, tenant names, organisation names, account ids or addresses in the repository.

| Suite | What it proves | Runs as | Gate |
|---|---|---|---|
| LL1 roles | Each identity signs in by redirect through Entra. The admin sees the admin nav, a member does not, and an identity with no Wardyn role is refused | Playwright | `WARDYN_LIVE_ENTRA=1` |
| LL2 Azure DevOps | A member who signed in once, with the credential captured, launches a run that does an Azure DevOps REST read and `git ls-remote` | Go | `WARDYN_LIVE_ADO=1` |
| LL2b Azure DevOps, bounded | A run that starts with `code_read` reads, has its push refused and raised for approval, pushes once the harness approves `code_write` for the run. It gets 403 with no request raised for `repo_admin`, which is above the ceiling | Go | `WARDYN_LIVE_ADO_WRITE=1` |
| LL2c personal access token probe | Whether an app registration holding only `vso.pats` and `vso.pats_manage` can, as the signed-in person, create a multi-scope personal access token, use it over Basic and revoke it. It logs a verdict either way and revokes anything it mints | Go | `WARDYN_LIVE_ADO_PAT_PROBE=1` |
| LL3 Bedrock | One Claude Haiku 4.5 call on Identity Center role credentials in the capped member account, and the reply is checked | Go | `WARDYN_LIVE_BEDROCK=1` |
| LL3w Bedrock through Wardyn | A governed run on the per-user SSO lane, then a second run on the bearer lane at a denied model; see [below](#ll3w-what-it-proves) | Go | `WARDYN_LIVE_BEDROCK_WARDYN=1` |
| LL4 AWS SSO through Entra | The per-user AWS SSO device sign-in URL, taken through the console's own extractor, lands on an Entra sign-in page | Playwright | `WARDYN_LIVE_AWS_SSO=1` |
| LL5 Autonomy L0 | A non-interactive run at autonomy level L0 is refused with a 403, and an interactive run is admitted; see [below](#ll5-what-it-proves) | Go | `WARDYN_LIVE_AUTONOMY=1` |

Every variable is listed in [ENV.md](ENV.md#live-local-harness-opt-in-never-in-ci).

- **The kind-cluster walks are a separate, heavier opt-in surface** — not LL1-4, and not gated by a `WARDYN_LIVE_*` variable.
  - [`scripts/kind-sso-walk.sh`](../scripts/kind-sso-walk.sh) (default profile, `WARDYN_TEST_K8S=1`) runs nightly in CI ([`.github/workflows/nightly.yml`](../.github/workflows/nightly.yml), the `kind-sso-walk` job).
  - Its Azure DevOps profile, `WARDYN_KIND_SSO_PROFILE=ado scripts/kind-sso-walk.sh` (recipe in [deploy/kind/sso/README.md](../deploy/kind/sso/README.md)), spins up a second kind cluster on its own port range and is **manual-only**: it has no scheduled nightly leg.
  - Run it by hand before a release that touches the Azure DevOps sign-in path.

### LL3w: what it proves

- A governed claude-code run whose model credential is Wardyn's own per-user AWS SSO capture (not the test process's own credentials) completes on the per-user SSO lane,
- on an allow-listed model, with a minted credential, attributed to the member, reply in the transcript;
- a second run on the bearer lane pointed at a denied model surfaces an `AccessDeniedException` sentence on `failure_hint`.

### LL5: what it proves

- A member whose governance profile caps this run's posture at L0 is refused a non-interactive run outright — 403 naming autonomy level L0 — before any run, identity or `run.exec` audit row exists;
- an interactive run at the same level is admitted and its managed-settings file is delivered.

## Rules

- **Secrets.** A secret is never an environment value.
  - The `*_FILE` variables and `WARDYN_LIVE_IDENTITIES_FILE` hold a path to a file outside the repository.
  - The Go suites refuse a path inside the checkout.
  - Keep these files mode `600`, for example under a `~/wardyn-*-live/` directory.
- **Output.** Everything the Go suites print goes through a redactor ([`internal/testlive/redact.go`](../internal/testlive/redact.go)).
  - It masks JWTs, `Bearer` values, AWS key ids, email addresses, GUIDs, 12-digit numbers and any long unbroken token-shaped string.
  - The browser suites assert on categories (Wardyn, Entra, other) rather than raw URLs, keep no trace, screenshot or video, and write their scratch output to the OS temp directory.
  - Nothing is written inside the repository.
- **Spend.** LL3 spends money and it is fenced in code ([`internal/testlive/bedrock.go`](../internal/testlive/bedrock.go)):
  - Credentials come only from IAM Identity Center role credentials for the account in `WARDYN_LIVE_BEDROCK_ACCOUNT_ID`, the capped member account.
    - Before any model call, the harness asks STS which account the credentials belong to and refuses unless it is exactly that account.
    - A management account is never usable: service control policies don't apply to it, so spend there escapes the cap.
    - There is no bearer-key or access-key path.
  - Models: Claude Haiku 4.5 and Amazon Nova Micro only, bare or behind a geographic inference profile.
    - Anything else is refused at start-up.
  - `max_tokens` is at most 32 per call.
  - At most `WARDYN_LIVE_BEDROCK_MAX_CALLS` calls per test process (default 5, hard maximum 20).
    - Past the budget, a call is refused before it is sent.
  - The account's own budget and service control policies stay the outer limit.
- LL3w spends money too, but the fence is partly the deployment's own, not this harness's own code:
  - it rides Wardyn's ordinary per-run Bedrock credential and asks for one short reply per run,
  - on a model the suite verifies AFTER the run completes is Claude Haiku 4.5 or Amazon Nova Micro (`testlive.ModelAllowed`, read off the run's own `run.bedrock.configure` audit row) —
  - there is no independent `max_tokens` fence on this path the way LL3's own direct SigV4 calls have one,
  - and no way to refuse a misconfigured Integration BEFORE the one call it makes (`GET /integrations` is not wrapped by the SDK this harness uses).
  - Configure the Integration this suite points at to a Haiku/Nova model to begin with; the post-hoc check catches a misconfiguration, it does not prevent the one call's worth of spend from it.
- LL5 spends nothing: `unattended_refused`'s whole proof is a 403 before a run exists,
  - and `interactive_agent_policy_delivered` sends NO task at all
  - (an interactive run with a task would seed the agent CLI at boot, before anyone attaches, spending at least one model call — see the test's own doc comment),
  - so no agent process ever starts.
- **One at a time.** Run live suites one at a time, and never alongside a heavy test gate.

## One-time setup

### Test identities

- In the Entra tenant, create one test identity per role and assign App Roles on Wardyn's app registration:
  - **admin**: the Admin App Role.
  - **member**: the Member App Role.
    - This one also needs access to the Azure DevOps test project.
  - **norole**: no App Role assignment.
- [`deploy/azure-entra-sso/`](../deploy/azure-entra-sso/) sets up the app registration and the role mapping.

### Record a storage state per identity

- The harness never types a password.
- Instead, you sign each identity in once by hand, and Playwright saves the browser session to a file:

```bash
cd ui
pnpm exec playwright open --save-storage="$HOME/wardyn-entra-live/state/admin.json" \
  "$WARDYN_LIVE_BASE_URL/auth/login"
```

- Sign in as that identity.
- When Entra asks "Stay signed in?", answer **Yes**.
- Once the console has loaded (or, for **norole**, shown the no-role message), close the window, and the file is written.
- Repeat for `member.json` and `norole.json`.
- Use a fresh window for each identity.
- When the Entra session lapses, LL1 skips and names the identity to re-record.

### Identities file

- `WARDYN_LIVE_IDENTITIES_FILE` points at a JSON file like this:

```json
{
  "admin":  { "storage_state": "/home/you/wardyn-entra-live/state/admin.json" },
  "member": { "storage_state": "/home/you/wardyn-entra-live/state/member.json",
              "api_token": "<the member's Wardyn API token>" },
  "norole": { "storage_state": "/home/you/wardyn-entra-live/state/norole.json" }
}
```

### Azure DevOps (LL2)

- The harness holds no Azure DevOps credential of its own.
- It uses only what Wardyn captured when the member signed in.
- Revoke any fixture PATs before you run this suite: nothing here reads them.

1. Sign in to the console as the member, and complete the Azure DevOps sign-in
   when the console asks for it. This is the capture.
2. Under Settings, create an API token for the member, and put it in the
   identities file as `member.api_token`.
3. Set `WARDYN_LIVE_ADO_ORG`, `WARDYN_LIVE_ADO_PROJECT` and
   `WARDYN_LIVE_ADO_REPO` to a repository the member can read.
4. In the same organisation, create a project named `Payments Platform` holding
   a Git repository named `Card Auth (v2).Service`, and give the member read
   access. LL2 runs a second time on it, because Azure DevOps names may carry
   spaces and punctuation. To use other names, set
   `WARDYN_LIVE_ADO_SPACED_PROJECT` and `WARDYN_LIVE_ADO_SPACED_REPO`.

### Azure DevOps, bounded (LL2b)

- LL2b uses LL2's member, organisation, project and repository.
- It pushes one branch, `wardyn/<run-id>/ll2b`, and deletes it again, so the member needs Contribute on the repository.
- The branch sits in the run's own namespace on purpose: a push to any other ref is outside the run's own branch and is refused outright unless the run's policy sets `git_push_any_branch`.
- The deployment needs:
  - an `entra` provider row for the organisation whose `default_profile`
    holds `code_read` and whose `capability_ceiling` holds `code_write` but not
    `repo_admin`, for example `["code_read", "project_read", "code_write", "pr"]`;
  - `first_use_approval: deny_with_review` in the policy the member's runs get
    ([`deploy/kind/sso/default-policy.json`](../deploy/kind/sso/default-policy.json) sets it).
- If it fails, the message maps the run's exit code to the step that went wrong.

### Personal access token probe (LL2c)

- LL2c is the measurement Wardyn's per-run tokens (`minted_pat`) rest on:
  - can an application that is not a Microsoft client, holding a user-delegated Entra token that carries **only** `vso.pats` and `vso.pats_manage`, list, create, use and revoke a personal access token through the token lifecycle API?
- An earlier measurement (2026-09-22) said no, with an app that held `vso.pats` and `vso.tokens`.
- It was a scope problem, not a limit of the API.
- The probe uses a separate, throwaway public-client app registration, not Wardyn's own, so the measurement does not depend on Wardyn's sign-in app (which holds these permissions only in `minted_pat` mode, and is confidential).
- Register it, and delete it afterwards.
- Record first what type the tenant publishes for the two scopes (`User` or `Admin`); it decides whether a person's connect needs admin consent:

```bash
az login --tenant "$TENANT_ID" --allow-no-subscriptions
ADO=499b84ac-1321-427f-aa17-267ca6975798        # the Azure DevOps API resource
az ad sp show --id $ADO --query "oauth2PermissionScopes[?starts_with(value,'vso.pat')].{scope:value,id:id,type:type}" -o table
APP=$(az ad app create --display-name wardyn-pat-probe --public-client-redirect-uris http://localhost \
  --query appId -o tsv)                          # -> WARDYN_LIVE_ADO_PAT_PROBE_CLIENT_ID
az ad sp create --id $APP
sleep 60                                          # let the service principal replicate before the grant
az ad app permission add --id $APP --api $ADO --api-permissions <id of vso.pats>=Scope <id of vso.pats_manage>=Scope
az ad app permission grant --id $APP --api $ADO --scope "vso.pats vso.pats_manage"
```

- Run it with `-v`, asking for both scopes:

```bash
WARDYN_LIVE_ADO_PAT_PROBE=1 WARDYN_LIVE_ADO_ORG=<org> \
WARDYN_LIVE_ADO_PAT_PROBE_TENANT_ID=<tenant> WARDYN_LIVE_ADO_PAT_PROBE_CLIENT_ID=$APP \
WARDYN_LIVE_ADO_PAT_PROBE_SCOPE="$ADO/vso.pats $ADO/vso.pats_manage" \
go test -tags live -count=1 -v -timeout 10m -run TestLiveADOPATMintProbe ./internal/testlive/
```

- It logs a sign-in URL: open it in a browser, sign in as the person to measure, and consent.
- It lists the person's tokens, creates one scoped by `WARDYN_LIVE_ADO_PAT_PROBE_PAT_SCOPE` (default `vso.code vso.project`, scopes separated by a single space), reads `https://dev.azure.com/<org>/_apis/projects` with it over Basic, and revokes it, whatever the read answered.
- The verdict line reads:
  - `VERDICT: MINT WORKS; use HTTP <n>; revoked HTTP <n>`: the revoke answered 200 or 204;
  - `VERDICT: MINT WORKS, REVOKE FAILED (HTTP <n>)`, and the test fails: revoke the token by hand under
    **Personal access tokens**;
  - `VERDICT: MINT REFUSED …`, with Azure DevOps' own error.
- If both scopes are refused, run it once more with `WARDYN_LIVE_ADO_PAT_PROBE_SCOPE=$ADO/user_impersonation` as the control.
- A sign-in that fails with `AADSTS65001` (consent), `AADSTS650053` (unknown scope), `AADSTS50011` (redirect), `AADSTS7000218` (allow public client flows on the app), `AADSTS700016` (app not found) or `AADSTS700025` (a secret presented by a public client) is an inconclusive run, not a verdict.
- Results, on a test organisation, 2026-09-30.
- "Restrict personal access token (PAT) creation" was off, and the tenant's full-scope and lifespan policies were left as they were:

| Signed in as | Granted scopes | Result |
|---|---|---|
| The tenant's administrator account (about 02:25 UTC) | `vso.pats`, `vso.pats_manage` | List 200. `MINT WORKS`. Revoke 204 |
| The tenant's member account (about 03:33 UTC) | `vso.pats`, `vso.pats_manage` | Created `vso.code vso.project` (a single space between scopes). Basic use `GET _apis/projects` 200. Revoke 204 |

- So a token carrying only the two token permissions creates a PAT of any other scope, the PAT works over Basic with several scopes, and a member account can do all of it.
- Not measured by these runs: whether the organisation policy "Restrict personal access token (PAT) creation" blocks creation through the API and in what shape, and what the lifespan policy answers to a 364-day request.
- The `type` (`User` or `Admin`) the tenant publishes for the two scopes is not recorded here.

### AWS (LL3, LL4)

- The AWS identity source is the same Entra tenant, and the Bedrock caller is a test identity's permission set on the capped member account.

1. Configure an AWS CLI SSO profile for that permission set on the member
   account, then sign in once with `aws sso login --profile <profile>`. The
   sign-in goes through Entra.
2. Point `WARDYN_LIVE_AWS_SSO_TOKEN_FILE` at the cache file that login wrote
   under `~/.aws/sso/cache/`. It is the one whose `startUrl` is your start
   URL. When it expires, LL3 fails and tells you to sign in again.

### Bedrock through Wardyn (LL3w)

- Unlike LL3, this suite never holds AWS credentials of its own — it drives the member's own Wardyn API token and Wardyn's own captured AWS SSO session.

1. Sign in to the console as the member and complete the AWS sign-in for this
   install's Bedrock SSO model provider (Settings → Model providers).
2. Set `WARDYN_LIVE_BEDROCK_WARDYN_MODEL_PROVIDER` to that provider's id
   (`GET /api/v1/model-providers`, or the Model providers page).
3. Optional, for the forced-`AccessDenied` half: set up a SECOND Bedrock provider, of the BEARER (API-key) kind specifically —
   - never the per-user AWS SSO kind, whose bedrock-runtime traffic is an opaque, un-MITM'd tunnel and can never surface this hint —
   - on a model this capped account's service control policy denies, add the member's own key to it, and set `WARDYN_LIVE_BEDROCK_WARDYN_DENIED_MODEL_PROVIDER` to its id.

   Unset, that half alone skips, named.

- `converse_through_wardyn` proves the run's credential mint is a PER-USER capture owned by this member —
  - never the operator's SHARED session, which produces the same `run.bedrock.configure` mode and would otherwise pass — by reading the mint's own scope snapshot (`credential_source`, `owner_subject`).
- `owner_subject` is compared for exact equality against this member's own `GET /me` principal;
  - both are `run.CreatedBy`/the token `sub` read the same way outside local mode (see `BedrockWardynRunProvesPerUserSSO`'s own doc comment for the exact call chain),
  - but this has not been independently confirmed against a live row —
  - if a real deployment spells the two differently, this subtest FAILS rather than silently passing on the wrong identity.
- Its message does not print either value: a token `sub` can be email-shaped, and this suite prints no value it does not have to, the same reason `RunCreatedByIsMember` prints neither `created_by` nor the principal it compares against.
- Diagnosing a real mismatch means reading the two values yourself, off the run's own `run.bedrock.configure`/`credential.mint` audit rows and `GET /me`.

### Autonomy L0 (LL5)

- The member needs a governance profile assignment whose `AutonomyRubric` resolves this run's posture to L0 (an admin authors this once through `POST /api/v1/governance/profiles` and `/governance/assignments` —
  - the suite does not author one itself, the same way LL2's Azure DevOps project is a fixture the suite assumes rather than creates).
- Set `WARDYN_LIVE_AUTONOMY_MODEL_PROVIDER` to a working model provider for that member, so the request reaches the autonomy gate instead of failing earlier on an unrelated missing-model-credential refusal.

## Running

- Start from a running Wardyn (a compose stack or a kind cluster) that signs in with the Entra tenant.
- The harness never starts one itself.

LL2 and LL3 (Go):

```bash
export WARDYN_LIVE_BASE_URL=https://wardyn.example.test
export WARDYN_LIVE_IDENTITIES_FILE=$HOME/wardyn-entra-live/identities.json

# LL2
WARDYN_LIVE_ADO=1 WARDYN_LIVE_ADO_ORG=... WARDYN_LIVE_ADO_PROJECT=... WARDYN_LIVE_ADO_REPO=... \
  go test -tags live -count=1 -v -run 'TestLiveADO$' ./internal/testlive/

# LL2b (same variables as LL2)
WARDYN_LIVE_ADO_WRITE=1 WARDYN_LIVE_ADO_ORG=... WARDYN_LIVE_ADO_PROJECT=... WARDYN_LIVE_ADO_REPO=... \
  go test -tags live -count=1 -v -run TestLiveADOBounded ./internal/testlive/

# LL2c (needs no running Wardyn)
WARDYN_LIVE_ADO_PAT_PROBE=1 WARDYN_LIVE_ADO_ORG=... \
WARDYN_LIVE_ADO_PAT_PROBE_TENANT_ID=... WARDYN_LIVE_ADO_PAT_PROBE_CLIENT_ID=... \
  go test -tags live -count=1 -v -timeout 10m -run TestLiveADOPATMintProbe ./internal/testlive/

# LL3
WARDYN_LIVE_BEDROCK=1 \
WARDYN_LIVE_BEDROCK_ACCOUNT_ID=... WARDYN_LIVE_BEDROCK_ROLE_NAME=... \
WARDYN_LIVE_BEDROCK_REGION=us-east-1 WARDYN_LIVE_AWS_SSO_REGION=... \
WARDYN_LIVE_AWS_SSO_TOKEN_FILE=$HOME/.aws/sso/cache/<file>.json \
  go test -tags live -count=1 -v -run TestLiveBedrock ./internal/testlive/

# LL3w
WARDYN_LIVE_BEDROCK_WARDYN=1 WARDYN_LIVE_BEDROCK_WARDYN_MODEL_PROVIDER=... \
  go test -tags live -count=1 -v -run TestLive_BedrockWardyn ./internal/testlive/

# LL5
WARDYN_LIVE_AUTONOMY=1 WARDYN_LIVE_AUTONOMY_MODEL_PROVIDER=... \
  go test -tags live -count=1 -v -run TestLive_AutonomyL0Enforced ./internal/testlive/
```

LL1 and LL4 (Playwright):

```bash
cd ui
# LL1
WARDYN_LIVE_ENTRA=1 pnpm exec playwright test -c playwright.live-local.config.ts entra-roles
# LL4
WARDYN_LIVE_AWS_SSO=1 WARDYN_LIVE_AWS_SSO_START_URL=... WARDYN_LIVE_AWS_SSO_REGION=... \
  pnpm exec playwright test -c playwright.live-local.config.ts aws-sso-entra
```

- The parts that need no live service run in the normal test suite:
  - `go test ./internal/testlive/` covers the redactor, the configuration limits, the SigV4 signer against AWS's published test vector, and the account refusal,
  - which uses a local fake STS that answers with the wrong account and checks that no model call is made.
- It also covers LL3w's and LL5's own grading logic hermetically —
  - `AutonomyL0RefusalOK`, `AgentPolicyDeliveredOK`, `BedrockWardynRunProvesPerUserSSO`, `RunCreatedByIsMember`, `TranscriptContainsReply` and `BedrockWardynForcedFaultOK` are pure functions, unit-tested against both a genuine and a deliberately wrong-shaped fixture,
  - so the live suites' PASS is never just "got a 403" or "the run ended" but the specific shape each proof requires.
