# Live-local tests

A small set of tests that run against real services: an Entra tenant, an Azure
DevOps organisation, and an AWS account with IAM Identity Center and Bedrock.
They exist to prove what the hermetic fakes can't. They are opt-in and local
only.

- They never run in CI or in `make ci`. The Go suites build only with
  `-tags live`. The browser suites have their own Playwright config, which no
  other config or script picks up.
- Every suite skips unless its own gate variable is `1`. The skip message
  names the variables it needs and never prints a value.
- Only code is committed. There are no recorded responses, fixtures, tenant
  names, organisation names, account ids or addresses in the repository.

| Suite | What it proves | Runs as | Gate |
|---|---|---|---|
| LL1 roles | Each identity signs in by redirect through Entra. The admin sees the admin nav, a member does not, and an identity with no Wardyn role is refused | Playwright | `WARDYN_LIVE_ENTRA=1` |
| LL2 Azure DevOps | A member who signed in once, with the credential captured, launches a run that does an Azure DevOps REST read and `git ls-remote` | Go | `WARDYN_LIVE_ADO=1` |
| LL2b Azure DevOps, bounded | A run that starts with `read` reads, has its push refused and raised for approval, pushes once the harness approves `code_write` for the run, and gets 403 with no request raised for `repo_admin`, which is above the ceiling | Go | `WARDYN_LIVE_ADO_WRITE=1` |
| LL2c personal access token probe | Whether a third-party app registration can mint a personal access token through the token lifecycle API. It logs a verdict either way and revokes anything it mints | Go | `WARDYN_LIVE_ADO_PAT_PROBE=1` |
| LL3 Bedrock | One Claude Haiku 4.5 call on Identity Center role credentials in the capped member account, and the reply is checked | Go | `WARDYN_LIVE_BEDROCK=1` |
| LL4 AWS SSO through Entra | The per-user AWS SSO device sign-in URL, taken through the console's own extractor, lands on an Entra sign-in page | Playwright | `WARDYN_LIVE_AWS_SSO=1` |

Every variable is listed in [ENV.md](ENV.md#live-local-harness-opt-in-never-in-ci).

## Rules

**Secrets.** A secret is never an environment value. The `*_FILE` variables
and `WARDYN_LIVE_IDENTITIES_FILE` hold a path to a file outside the
repository. The Go suites refuse a path inside the checkout. Keep these files
mode `600`, for example under a `~/wardyn-*-live/` directory.

**Output.** Everything the Go suites print goes through a redactor
(`internal/testlive/redact.go`). It masks JWTs, `Bearer` values, AWS key ids,
email addresses, GUIDs, 12-digit numbers and any long unbroken token-shaped
string. The browser suites assert on categories (Wardyn, Entra, other) rather
than raw URLs, keep no trace, screenshot or video, and write their scratch
output to the OS temp directory. Nothing is written inside the repository.

**Spend.** Only LL3 spends money, and it is fenced in code
(`internal/testlive/bedrock.go`):

- Credentials come only from IAM Identity Center role credentials for the
  account in `WARDYN_LIVE_BEDROCK_ACCOUNT_ID`, the capped member account.
  Before any model call, the harness asks STS which account the credentials
  belong to and refuses unless it is exactly that account. A management
  account is never usable: service control policies don't apply to it, so
  spend there escapes the cap. There is no bearer-key or access-key path.
- Models: Claude Haiku 4.5 and Amazon Nova Micro only, bare or behind a
  geographic inference profile. Anything else is refused at start-up.
- `max_tokens` is at most 32 per call.
- At most `WARDYN_LIVE_BEDROCK_MAX_CALLS` calls per test process (default 5,
  hard maximum 20). Past the budget, a call is refused before it is sent.
- The account's own budget and service control policies stay the outer
  limit.

**One at a time.** Run live suites one at a time, and never alongside a heavy
test gate.

## One-time setup

### Test identities

In the Entra tenant, create one test identity per role and assign App Roles
on Wardyn's app registration:

- **admin**: the Admin App Role.
- **member**: the Member App Role. This one also needs access to the Azure
  DevOps test project.
- **norole**: no App Role assignment.

`deploy/azure-entra-sso/` sets up the app registration and the role mapping.

### Record a storage state per identity

The harness never types a password. Instead, you sign each identity in once
by hand, and Playwright saves the browser session to a file:

```bash
cd ui
pnpm exec playwright open --save-storage="$HOME/wardyn-entra-live/state/admin.json" \
  "$WARDYN_LIVE_BASE_URL/auth/login"
```

Sign in as that identity. When Entra asks "Stay signed in?", answer **Yes**.
Once the console has loaded (or, for **norole**, shown the no-role message),
close the window, and the file is written. Repeat for `member.json` and
`norole.json`. Use a fresh window for each identity.

When the Entra session lapses, LL1 skips and names the identity to
re-record.

### Identities file

`WARDYN_LIVE_IDENTITIES_FILE` points at a JSON file like this:

```json
{
  "admin":  { "storage_state": "/home/you/wardyn-entra-live/state/admin.json" },
  "member": { "storage_state": "/home/you/wardyn-entra-live/state/member.json",
              "api_token": "<the member's Wardyn API token>" },
  "norole": { "storage_state": "/home/you/wardyn-entra-live/state/norole.json" }
}
```

### Azure DevOps (LL2)

The harness holds no Azure DevOps credential of its own. It uses only what
Wardyn captured when the member signed in. Revoke any fixture PATs before you
run this suite: nothing here reads them.

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

LL2b uses LL2's member, organisation, project and repository. It pushes one
branch, `wardyn/<run-id>/ll2b`, and deletes it again, so the member needs
Contribute on the repository. The branch sits in the run's own namespace on
purpose: a push to any other ref counts as a protected-ref move and asks for
`policy_bypass`, not `code_write`. The deployment needs:

- an `entra` provider row for the organisation with `default_profile`
  `["read"]` and a `capability_ceiling` that holds `code_write` but not
  `repo_admin`, for example `["read", "code_write", "pr"]`;
- `first_use_approval: deny_with_review` in the policy the member's runs get
  (`deploy/kind/sso/default-policy.json` sets it).

If it fails, the message maps the run's exit code to the step that went wrong.

### Personal access token probe (LL2c)

Wardyn dropped its minted-token mode because a measurement said Azure DevOps
mints personal access tokens only for Microsoft's own clients. Microsoft's
documentation says a user-delegated Entra token with `vso.pats` may mint.
LL2c re-measures it on your tenant.

It must not use Wardyn's app registration: consent decides a token's scopes,
so `vso.pats` consented there would ride along in every run's token. Register
a separate public-client app for it, and delete it afterwards:

```bash
az ad app create --display-name wardyn-pat-probe --public-client-redirect-uris http://localhost \
  --query appId -o tsv            # -> WARDYN_LIVE_ADO_PAT_PROBE_CLIENT_ID
```

Run it with `-v`. It logs a sign-in URL: open it in a browser, sign in as the
member and consent. The verdict line reads `MINT WORKS` or `MINT REFUSED`,
with Azure DevOps' own error. To measure the full-access scope as well, run it
again with
`WARDYN_LIVE_ADO_PAT_PROBE_SCOPE=499b84ac-1321-427f-aa17-267ca6975798/user_impersonation`.

### AWS (LL3, LL4)

The AWS identity source is the same Entra tenant, and the Bedrock caller is a
test identity's permission set on the capped member account.

1. Configure an AWS CLI SSO profile for that permission set on the member
   account, then sign in once with `aws sso login --profile <profile>`. The
   sign-in goes through Entra.
2. Point `WARDYN_LIVE_AWS_SSO_TOKEN_FILE` at the cache file that login wrote
   under `~/.aws/sso/cache/`. It is the one whose `startUrl` is your start
   URL. When it expires, LL3 skips and tells you to sign in again.

## Running

Start from a running Wardyn (a compose stack or a kind cluster) that signs in
with the Entra tenant. The harness never starts one itself.

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

The parts that need no live service run in the normal test suite:
`go test ./internal/testlive/` covers the redactor, the configuration limits,
the SigV4 signer against AWS's published test vector, and the account
refusal, which uses a local fake STS that answers with the wrong account and
checks that no model call is made.
