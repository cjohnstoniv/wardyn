# How a credential reaches a run

- A run needs credentials for the services it calls. This page says where each one is while the run works, and why.
- For most kinds the egress proxy adds the credential to the outbound request, and Wardyn places no copy in the sandbox.
- A policy or a component can deliver a secret into the sandbox as an environment variable or a file, and two Git modes place a credential there too.
- Each shape leaves a residual; [Residuals](#residuals) states them and what bounds each.
- How secrets are stored and encrypted is a different question: see [Secrets and keys](operations/secrets-and-keys.md).

## The idea

- A destination checks the credential on the request it receives. It does not check which program wrote it there.
- Wardyn sends a run's outbound traffic through that run's egress proxy (invariant 3 in [`ARCHITECTURE.md`](../ARCHITECTURE.md)).
- So where the proxy can read a request, it can add the credential on the way out, and the workload makes the call without holding the value.
- Where the workload's own program must read the value, or sign with it, the value has to be inside the sandbox.

A credential therefore reaches a destination in one of four shapes:

- **Added at the proxy.** The value stays in the control plane's secret store and the proxy's memory.
- **Minted for the run.** The control plane creates a short-lived credential for this run, and the proxy adds it.
- **Delivered inside.** The value is written into the sandbox as a variable or a file.
- **Made inside.** The workload derives the credential itself from something else.

## Why the proxy can add some credentials and not others

### A credential carried in a request header

- Examples: an API key, an OAuth bearer or subscription token, a brokered Git token, a model-provider key.
- The destination needs to see the value on the HTTP request, and nowhere else.
- The workload's program needs to make the request. It does not need the real value to do that.
- The proxy can add a header only to a request it can read. It reads a request in three places:
  - on its own plain-HTTP routes, which the sandbox is pointed at for model calls and brokered Git ([`internal/egress/proxy/llm_routes.go#Proxy.handleLLMAnthropic`](../internal/egress/proxy/llm_routes.go), [`internal/egress/proxy/git_broker.go#Proxy.handleGitBroker`](../internal/egress/proxy/git_broker.go));
  - inside a TLS connection it terminates with a per-run certificate authority, for the hosts its interception rule admits ([`internal/egress/proxy/mitm.go#Proxy.isMITMHost`](../internal/egress/proxy/mitm.go));
  - on a plain-HTTP forward request to a host that has an injection rule ([`internal/egress/proxy/inject.go#injector.apply`](../internal/egress/proxy/inject.go)).
- An HTTPS connection to any other host is an opaque tunnel: the proxy sees the host name and adds nothing ([`internal/egress/proxy/inject.go#InjectionConfig`](../internal/egress/proxy/inject.go)).
- Before it sets its own header, the proxy removes the credential headers the sandbox put on the request ([`internal/egress/proxy/inject.go#stripSandboxCredentials`](../internal/egress/proxy/inject.go)).
- The proxy asks the control plane for the value with the run's token, on a route that has no forward from the sandbox ([`internal/api/injection.go#Server.handleInternalInjection`](../internal/api/injection.go)).
- The sandbox holds an inert placeholder where a tool insists on a value, and otherwise nothing; [the table](#every-kind-side-by-side) says which.

Because the control plane answers each time the proxy asks, it can also renew or replace the credential while the run works:

- On an Azure DevOps `bearer` row, an Entra access token is redeemed from the person's stored refresh token ([`internal/types/secret_sentinels.go#ADOEntraAccessTokenSecret`](../internal/types/secret_sentinels.go)).
- A captured AWS sign-in session is renewed by the control plane, the one party that can store the rotated refresh token ([`internal/api/awssso_refresh.go`](../internal/api/awssso_refresh.go)).
- A GitHub App installation token is minted for the repositories the run was granted ([`internal/broker/github.go#githubMinter.MintInstallationToken`](../internal/broker/github.go)).
- On an Azure DevOps `minted_pat` row, a token is created for the run and revoked on pause and at the run's end ([How a run's token lives](AZURE-DEVOPS.md#how-a-runs-token-lives-minted_pat)).
- A stored key is read again on a short lease ([`internal/api/injection.go#Server.storedKeyExpiry`](../internal/api/injection.go)). An approval-gated grant is read once per approval instead.
- The proxy asks again from `5m` before the lease ends, on the next request ([`internal/egress/proxy/inject.go#injectRefreshMargin`](../internal/egress/proxy/inject.go)).
- A definitive refusal, such as a key removed from the store, stops the header at once.
- While the control plane does not answer (a `503` or a transport error), the proxy keeps adding the last good value for up to `15m` past the lease ([`internal/egress/proxy/inject.go#injEntry.lastGood`](../internal/egress/proxy/inject.go)).

### A value the program reads itself

- Examples: a command-line tool that reads `$TOKEN`, or a tool that opens a credentials file.
- The program reads the value into its own memory. It often does something with it the proxy does not see:
  - it signs a request locally;
  - it speaks a protocol that is not HTTP;
  - it calls a host that has no injection rule.
- A value in a process's memory is in the sandbox, and no step at the proxy changes that.
- Wardyn delivers such a value with an `env_secret` or a `file_secret` grant, resolved when the run is dispatched ([`internal/types/types.go#GrantEnvSecret`](../internal/types/types.go), `GrantFileSecret`).

Who may deliver one:

- **On an operator's own run**, the policy may carry either grant; a grant that names a reserved platform secret is not delivered ([`internal/api/runs_dispatch_secrets.go#Server.resolveEnvSecretGrants`](../internal/api/runs_dispatch_secrets.go)).
- An operator is the admin role, the admin token, or any caller where no sign-in is configured ([`internal/api/http.go#Server.isOperator`](../internal/api/http.go)).
- A device, a portal acting for a person and the identity provider's SCIM connector are never operators, even where no sign-in is configured ([`internal/api/delegation.go#neverOperator`](../internal/api/delegation.go)).
- **On anyone else's run**, both grants are dropped from the policy unless the operator sets `WARDYN_ALLOW_USER_ENV_SECRET` ([ENV.md](ENV.md)), whichever route the policy arrives by ([`internal/api/inline_policy_bounds.go#Server.boundEnvSecretPosture`](../internal/api/inline_policy_bounds.go)).
- With that switch set, a member's own inline grant must still match a pairing in the member's ceiling ([`internal/api/inline_policy_secrets.go#Server.filterUserGrants`](../internal/api/inline_policy_secrets.go)).
- **A component** delivers the launching person's own secret: its variable and file grants are `owner_only`, with no fallback to an operator secret of the same name ([`internal/api/components_run.go#componentGrant`](../internal/api/components_run.go)).
- Defining a component of one's own is on by default. Where the `feature` capability is not enforced, everyone may until a deny row names `custom_component`; where it is enforced, a person needs an allow row ([`internal/api/capabilities.go#featureCustomComponent`](../internal/api/capabilities.go)).
- An organisation can refuse variable and file delivery by every component with `components.deny_resident_delivery` ([`internal/types/site_config.go#ComponentSettings`](../internal/types/site_config.go)).

What Wardyn does for a delivered value:

- It records the value for output masking before the sandbox is created ([`internal/api/runs_dispatch_files.go#Server.completeMaskManifestWithFileSecrets`](../internal/api/runs_dispatch_files.go)).
- It audits the delivery by variable or file name and by secret name, without the value (`run.env_secret.resolve`, `run.file_secret.resolve`).
- It writes a file under `/run/wardyn/secrets` ([`internal/runner/managed_files.go#ComponentSecretDir`](../internal/runner/managed_files.go)).
  - On Docker the file is mode `0400`, owned by the workload's user ([`internal/runner/docker/managed_files.go#managedFilesTar`](../internal/runner/docker/managed_files.go)).
  - On Kubernetes it is mode `0440`, read through the pod's file-system group, which every process in the workload's pod carries ([`internal/runner/k8s/managed_files.go#agentSecretItemMode`](../internal/runner/k8s/managed_files.go), `applyAgentSecretFSGroup`).
- A `file_secret` grant on a runner that cannot deliver a file fails the run ([`internal/api/runs_dispatch_files.go#Server.fileSecretRunnerDelivers`](../internal/api/runs_dispatch_files.go)).
- On Kubernetes, a delivered file beside a shared drive is refused ([`internal/runner/k8s/managed_files.go#errAgentSecretBesideShare`](../internal/runner/k8s/managed_files.go)).
- It skips a grant it cannot honour, and leaves the variable or file absent; it does not set a blank ([`internal/api/runs_dispatch_files.go#Server.resolveFileSecretGrants`](../internal/api/runs_dispatch_files.go)).

What it does not do:

- There is no mint, no expiry and no revocation. The broker refuses both kinds at its mint route ([`internal/broker/broker.go#Broker.mintKind`](../internal/broker/broker.go)).
- Ending the run stops the process. It does not un-disclose a value the workload already read.

### A Git credential

Git over HTTPS can be brokered, because the proxy can serve the Git request itself:

- **GitHub App lane (`github_token`).** The sandbox's Git is pointed at the proxy's `/wardyn/gh/` route. The proxy mints a repo-scoped installation token and sets it on the outbound request ([`internal/egress/proxy/git_broker.go#Proxy.handleGitBroker`](../internal/egress/proxy/git_broker.go)).
- **Token broker (`git_pat`).** With `WARDYN_GIT_PAT_BROKER=on`, the default, the proxy serves `/wardyn/git/<host>/` and sets Basic auth on the outbound request ([`internal/egress/proxy/pat_broker.go#Proxy.handlePATBroker`](../internal/egress/proxy/pat_broker.go)).
- **Azure DevOps per-person lane.** The proxy adds the person's own credential to Git requests, and on Azure DevOps Services to REST requests ([`internal/egress/proxy/pat_broker_entra.go#Proxy.serveADOGit`](../internal/egress/proxy/pat_broker_entra.go)). The row's `token_mode` picks which credential: see [Choosing how people connect](AZURE-DEVOPS.md#choosing-how-people-connect).
- On the first two lanes the proxy refuses a mint request from the sandbox for the same grant ([`internal/egress/proxy/local_routes.go#Proxy.handleBrokerMint`](../internal/egress/proxy/local_routes.go)).

Two modes place a Git credential inside the sandbox:

- **`ssh_key`.** The SSH client authenticates inside the sandbox and reads the key from a file. Git's credential helper covers HTTP only, so the proxy has no request to add a credential to ([`internal/broker/broker_mint_kinds.go#Broker.mintSSHKey`](../internal/broker/broker_mint_kinds.go)).
- The run's start script writes that key at mode `0400`, clones, then wipes it ([`deploy/images/claude-code/agent-run#wipe_ssh_grants`](../deploy/images/claude-code/agent-run)).
- The grant id stays in `WARDYN_SSH_GRANTS` for the whole run, and the proxy's mint route refuses only brokered GitHub and token-broker grants ([`internal/egress/proxy/local_routes.go#Proxy.handleBrokerMint`](../internal/egress/proxy/local_routes.go)).
- So for a grant that is not approval-gated, the key file is wiped after the clone and the grant is re-mintable until the run ends ([`internal/broker/mint_prepare.go#Broker.checkedMintRow`](../internal/broker/mint_prepare.go)).
- **`git_pat` with `WARDYN_GIT_PAT_BROKER=off`.** The helper inside the sandbox obtains the token and prints it to Git ([`cmd/wardyn-git-helper/main.go`](../cmd/wardyn-git-helper/main.go)).
- For a forge the run holds a `github_token` grant for, a co-declared `ssh_key` or `git_pat` grant is refused when the policy is written and withheld at dispatch ([`internal/api/policy.go#validateGrantLaneExclusivity`](../internal/api/policy.go), [`internal/api/runs_dispatch_gitbroker.go#dropBrokeredGrants`](../internal/api/runs_dispatch_gitbroker.go)).

### A cloud credential made inside

- On a `bedrock_sso` model provider, the AWS SDK inside the sandbox exchanges the sign-in session for short-lived role credentials and signs each request with them.
- The signature is computed in the workload's process, so those role credentials are inside the sandbox on every such run ([`internal/api/credential_residency.go#kindResidency`](../internal/api/credential_residency.go)).
- The session they come from is a header credential. With `WARDYN_AWS_SSO_PROXY_INJECT=on`, the default, the proxy adds it and the sandbox's token cache holds a placeholder ([`internal/api/runs_bedrock.go#awsSSOCacheFileContents`](../internal/api/runs_bedrock.go)).
- A sign-in sandbox is the other case: its purpose is to obtain a credential that does not exist yet, so there is nothing to add ([`internal/api/provider_signin.go#Server.handleProviderSignIn`](../internal/api/provider_signin.go)).

## Every kind, side by side

- "Masked" means the value's exact bytes are replaced in the output Wardyn relays or stores; [Residuals](#residuals) says where that stops.
- "No copy placed" describes what Wardyn delivers. It is not a promise about what a destination sends back.

| Credential | The real value lives | The sandbox holds | Readable inside by | Masked in output | How it ends | Residual |
|---|---|---|---|---|---|---|
| Model key or endpoint token: provider kinds `anthropic_api_key`, `openai_api_key`, `custom_endpoint`, `bedrock_bearer` ([`internal/types/model_provider.go`](../internal/types/model_provider.go)) | The person's own row in the secret store, and the proxy's memory ([`internal/api/injection_provider_key.go#Server.resolveProviderKeyInjection`](../internal/api/injection_provider_key.go)) | The placeholder `wardyn-proxy-injected` in the `claude-code` or `codex-cli` key variable; the `none` harness is wired with no model credential ([`internal/api/runs_dispatch_provider.go#Server.applyProviderEnv`](../internal/api/runs_dispatch_provider.go), [`internal/api/provider_bedrock.go#Server.providerBedrockTransport`](../internal/api/provider_bedrock.go)) | Any process reads the placeholder; the proxy strips it from the request ([`internal/egress/proxy/inject.go#stripSandboxCredentials`](../internal/egress/proxy/inject.go)) | Yes, verbatim ([`internal/api/mask_manifest.go#Server.refuseUnmasked`](../internal/api/mask_manifest.go)) | Good for `15m`, then re-checked against the provider record ([`internal/api/injection_provider_key.go#providerKeyRecheck`](../internal/api/injection_provider_key.go)); while the control plane does not answer, the last good value for up to `15m` more ([`internal/egress/proxy/inject.go#injEntry.lastGood`](../internal/egress/proxy/inject.go)) | The destination can echo the header back ([`internal/egress/proxy/local_routes.go#relay`](../internal/egress/proxy/local_routes.go)) |
| Subscription sign-in token: provider kind `anthropic_subscription` ([`internal/types/model_provider.go`](../internal/types/model_provider.go)) | The person's captured sign-in, in their own namespace ([`internal/api/provider_subscription.go#Server.resolveProviderSubscriptionInjection`](../internal/api/provider_subscription.go)) | An inert sentinel credentials file, passed in a variable ([`internal/api/harnesscred.go#managedSentinelCredsB64`](../internal/api/harnesscred.go)) | Any process reads the sentinel, which carries no secret ([`internal/api/harnesscred.go#managedSentinelAccessToken`](../internal/api/harnesscred.go)) | Yes, verbatim ([`internal/api/mask_manifest.go#Server.refuseUnmasked`](../internal/api/mask_manifest.go)) | Read again on the stored-key lease, or at the token's own expiry when that is sooner ([`internal/api/injection.go#Server.subscriptionLease`](../internal/api/injection.go)) | Header echo ([`internal/egress/proxy/local_routes.go#relay`](../internal/egress/proxy/local_routes.go)) |
| Entra sign-in for a model endpoint: provider kind `azure_foundry` ([`internal/types/model_provider.go`](../internal/types/model_provider.go)) | Nowhere: a provider of this kind cannot be saved in this release ([`internal/api/model_providers_azure.go#azureFoundryGateReady`](../internal/api/model_providers_azure.go)) | Nothing ([`internal/api/model_providers_azure.go#azureFoundryGateReady`](../internal/api/model_providers_azure.go)) | Nothing to read ([`internal/api/model_providers_azure.go#azureFoundryGateReady`](../internal/api/model_providers_azure.go)) | Not applicable ([`internal/api/model_providers_azure.go#azureFoundryGateReady`](../internal/api/model_providers_azure.go)) | Not applicable ([`internal/api/model_providers_azure.go#azureFoundryGateReady`](../internal/api/model_providers_azure.go)) | None while the kind is switched off ([`internal/api/model_providers_azure.go#azureFoundryGateReady`](../internal/api/model_providers_azure.go)) |
| AWS sign-in session: provider kind `bedrock_sso` ([`internal/types/model_provider.go`](../internal/types/model_provider.go)) | The person's captured session in the store; the proxy adds it as `x-amz-sso_bearer_token` ([`internal/api/injection_awssso.go#Server.resolveAWSSSOInjection`](../internal/api/injection_awssso.go)) | A generated AWS config, and a token cache with a placeholder; the real access token when `WARDYN_AWS_SSO_PROXY_INJECT=off` ([`internal/api/runs_bedrock.go#awsSSOCacheFileContents`](../internal/api/runs_bedrock.go)) | Code in the sandbox, in the `off` mode ([`internal/api/runs_dispatch_llm.go#Server.applyBedrockTransport`](../internal/api/runs_dispatch_llm.go)) | Yes, verbatim ([`internal/api/mask_manifest.go#Server.refuseUnmasked`](../internal/api/mask_manifest.go)) | Wardyn cannot revoke the session; see the captured-AWS-SSO row in the [threat model](../threatmodel/THREAT-MODEL.md#51a-llm-egress-content-inspection--the-honest-claims-contract) | The role credentials in the next row ([`internal/api/credential_residency.go#kindResidency`](../internal/api/credential_residency.go)) |
| AWS role credentials made from that session, on every `bedrock_sso` run ([`internal/api/credential_residency.go`](../internal/api/credential_residency.go)) | In the workload's own AWS SDK ([`internal/api/credential_residency.go#kindResidency`](../internal/api/credential_residency.go)) | The live role credentials ([`internal/api/credential_residency.go#residencySandbox`](../internal/api/credential_residency.go)) | Code in the sandbox ([`internal/api/runs_dispatch_llm.go#Server.applyBedrockTransport`](../internal/api/runs_dispatch_llm.go)) | No: Wardyn does not see these values ([threat model](../threatmodel/THREAT-MODEL.md#51a-llm-egress-content-inspection--the-honest-claims-contract)) | At their own lifetime, set outside Wardyn ([threat model](../threatmodel/THREAT-MODEL.md#51a-llm-egress-content-inspection--the-honest-claims-contract)) | Bounded by the role's scope and lifetime, both set outside Wardyn ([threat model](../threatmodel/THREAT-MODEL.md#51a-llm-egress-content-inspection--the-honest-claims-contract)) |
| `api_key` grant: a stored key for one host and one header ([POLICIES.md](POLICIES.md#api_key)) | A stored secret: the run owner's row, then the operator's unless `owner_only` ([`internal/api/injection.go#Server.handleInternalInjection`](../internal/api/injection.go)) | No copy placed ([`internal/broker/broker_mint_kinds.go#Broker.mintAPIKey`](../internal/broker/broker_mint_kinds.go)) | No copy placed ([`internal/broker/broker_mint_kinds.go#Broker.mintAPIKey`](../internal/broker/broker_mint_kinds.go)) | Yes, verbatim ([`internal/api/mask_manifest.go#Server.refuseUnmasked`](../internal/api/mask_manifest.go)) | Good for `10m`, read once per approval if approval-gated ([`internal/api/injection.go#Server.storedKeyExpiry`](../internal/api/injection.go)); while the control plane does not answer, the last good value for up to `15m` more ([`internal/egress/proxy/inject.go#injEntry.lastGood`](../internal/egress/proxy/inject.go)) | Header echo. Added only where the proxy reads the request ([`internal/egress/proxy/inject.go#InjectionConfig`](../internal/egress/proxy/inject.go)) |
| Component secret delivered as a header ([`internal/types/component.go`](../internal/types/component.go)) | The person's own secret, or the operator's for a `shared` one ([`internal/api/runs_scm.go#injectionGrantRead`](../internal/api/runs_scm.go)) | No copy placed ([`internal/types/component.go#ComponentDeliveryHeader`](../internal/types/component.go)) | No copy placed ([`internal/api/components_run.go#componentGrant`](../internal/api/components_run.go)) | Yes, verbatim ([`internal/api/mask_manifest.go#Server.refuseUnmasked`](../internal/api/mask_manifest.go)) | Good for `10m` ([`internal/api/injection.go#Server.storedKeyExpiry`](../internal/api/injection.go)); while the control plane does not answer, the last good value for up to `15m` more ([`internal/egress/proxy/inject.go#injEntry.lastGood`](../internal/egress/proxy/inject.go)) | Header echo; for `shared`, of the operator's value ([`internal/egress/proxy/local_routes.go#relay`](../internal/egress/proxy/local_routes.go)) |
| `github_token` grant: a GitHub App installation token ([POLICIES.md](POLICIES.md#github_token)) | Minted by the control plane; held in the proxy's memory ([`internal/egress/proxy/git_broker.go#Proxy.handleGitBroker`](../internal/egress/proxy/git_broker.go)) | The grant's id, which the proxy refuses to mint for the sandbox ([`internal/egress/proxy/git_broker.go#Proxy.isBrokeredGitGrant`](../internal/egress/proxy/git_broker.go)) | No copy placed on a run brokered for at least one repository ([`internal/egress/proxy/local_routes.go#Proxy.handleBrokerMint`](../internal/egress/proxy/local_routes.go)) | Yes, verbatim ([`internal/broker/broker.go#Broker.maskMinted`](../internal/broker/broker.go)) | It expires within `1h`; Wardyn does not call GitHub's revoke ([`internal/broker/revoke.go#revokeNote`](../internal/broker/revoke.go)) | A token already minted lives to its expiry after the run ends ([`internal/broker/revoke.go#revokeNote`](../internal/broker/revoke.go)) |
| `git_pat` grant: a stored token for another forge ([POLICIES.md](POLICIES.md#git_pat)) | A stored secret; the proxy's memory while the broker is on ([`internal/egress/proxy/pat_broker.go#Proxy.handlePATBroker`](../internal/egress/proxy/pat_broker.go)) | No copy placed while the broker is on; the token itself with `WARDYN_GIT_PAT_BROKER=off` ([`internal/broker/broker_mint_kinds.go#Broker.mintGitPAT`](../internal/broker/broker_mint_kinds.go)) | In the `off` mode: Git, and a caller that posts the grant id to the mint route ([threat model](../threatmodel/THREAT-MODEL.md#51a-llm-egress-content-inspection--the-honest-claims-contract)) | Yes, verbatim ([`internal/broker/broker.go#Broker.maskMinted`](../internal/broker/broker.go)) | The operator rotates it at the forge; Wardyn cannot expire it ([`internal/broker/revoke.go#revokeNote`](../internal/broker/revoke.go)) | The token keeps the scope its forge issued; the broker narrows the run, not the token ([POLICIES.md](POLICIES.md#git_pat)) |
| Azure DevOps per-person credential: `minted_pat`, `bearer` or `own_pat` ([AZURE-DEVOPS.md](AZURE-DEVOPS.md#choosing-how-people-connect)) | The person's sign-in or pasted token in the store; the control plane resolves it for the proxy ([`internal/api/injection_ado.go#Server.resolveADOInjection`](../internal/api/injection_ado.go)) | An inert placeholder in `AZURE_DEVOPS_EXT_PAT` ([`internal/api/runs_dispatch_ado_inject.go#adoEntraPlaceholderEnv`](../internal/api/runs_dispatch_ado_inject.go)) | Any process reads the placeholder ([`internal/api/runs_dispatch_ado_inject.go#adoEntraPlaceholderValue`](../internal/api/runs_dispatch_ado_inject.go)) | Yes, verbatim ([`internal/api/mask_manifest.go#Server.refuseUnmasked`](../internal/api/mask_manifest.go)) | By mode: revoked at the run's end, renewed hourly, or not revocable by Wardyn ([AZURE-DEVOPS.md](AZURE-DEVOPS.md#choosing-how-people-connect)) | On `bearer` the token carries every scope the person consented to; the proxy's own check bounds the run ([`internal/types/secret_sentinels.go#ADOEntraAccessTokenSecret`](../internal/types/secret_sentinels.go)) |
| `env_secret` grant, or a component secret delivered as a variable ([POLICIES.md](POLICIES.md#env_secret)) | A stored secret, copied into the sandbox at dispatch ([`internal/api/runs_dispatch_secrets.go#Server.resolveEnvSecretGrants`](../internal/api/runs_dispatch_secrets.go)) | The value, in the variable the grant names ([`internal/api/runs_dispatch_secrets.go#Server.resolveEnvSecretGrants`](../internal/api/runs_dispatch_secrets.go)) | Anything running as the workload's user, for the whole run ([`internal/types/types.go#GrantEnvSecret`](../internal/types/types.go)) | Yes, verbatim ([`internal/api/mask_manifest.go#Server.maskDispatchValue`](../internal/api/mask_manifest.go)) | It does not: no expiry, nothing to revoke ([`internal/broker/broker.go#Broker.mintKind`](../internal/broker/broker.go)) | Read by any code in the sandbox, and sent wherever the run's policy allows ([`internal/types/types.go#GrantEnvSecret`](../internal/types/types.go)) |
| `file_secret` grant, or a component secret delivered as a file ([`internal/types/types.go`](../internal/types/types.go)) | A stored secret, copied into the sandbox at dispatch ([`internal/api/runs_dispatch_files.go#Server.resolveFileSecretGrants`](../internal/api/runs_dispatch_files.go)) | The value, in a file under `/run/wardyn/secrets` ([`internal/runner/managed_files.go#ComponentSecretDir`](../internal/runner/managed_files.go)) | Docker: the workload's user, mode `0400` ([`internal/runner/docker/managed_files.go#managedFilesTar`](../internal/runner/docker/managed_files.go)). Kubernetes: every process in the pod, mode `0440` ([`internal/runner/k8s/managed_files.go#agentSecretItemMode`](../internal/runner/k8s/managed_files.go)) | Yes: as stored, and without its trailing line break ([`internal/api/runs_dispatch_files.go#fileSecretRenderings`](../internal/api/runs_dispatch_files.go)) | It does not: no expiry, nothing to revoke ([`internal/broker/broker.go#Broker.mintKind`](../internal/broker/broker.go)) | Read by any code in the sandbox, and sent wherever the run's policy allows ([`internal/types/types.go#GrantFileSecret`](../internal/types/types.go)) |
| `ssh_key` grant: an SSH private key for Git ([POLICIES.md](POLICIES.md#ssh_key)) | A stored secret; minted to the run's start script ([`internal/broker/broker_mint_kinds.go#Broker.mintSSHKey`](../internal/broker/broker_mint_kinds.go)) | The key file at mode `0400` during the clone; the grant id for the whole run ([`deploy/images/claude-code/agent-run#provision_ssh_grants`](../deploy/images/claude-code/agent-run)) | The workload's user while the file exists, and any process that posts the grant id to the mint route unless the grant is approval-gated ([`internal/egress/proxy/local_routes.go#Proxy.handleBrokerMint`](../internal/egress/proxy/local_routes.go)) | Yes, verbatim ([`internal/broker/broker.go#Broker.maskMinted`](../internal/broker/broker.go)) | Wiped after the clone, and re-mintable until the run ends; the operator rotates the key at the forge ([`internal/broker/revoke.go#revokeNote`](../internal/broker/revoke.go)) | The wipe narrows the window and is not a bound ([`internal/broker/mint_prepare.go#Broker.checkedMintRow`](../internal/broker/mint_prepare.go); the `ssh_key` row in the [threat model](../threatmodel/THREAT-MODEL.md#51a-llm-egress-content-inspection--the-honest-claims-contract)) |
| The credential a sign-in sandbox exists to obtain ([`internal/api/provider_signin.go`](../internal/api/provider_signin.go)) | Nowhere yet: the sign-in creates it ([`internal/api/provider_signin.go#Server.handleProviderSignIn`](../internal/api/provider_signin.go)) | The new credential, for the sandbox's life; the capture copies it out and does not remove it ([`internal/api/provider_signin.go#Server.handleProviderSignInCapture`](../internal/api/provider_signin.go)) | Code in that sandbox, and the person at its terminal ([`internal/api/harnesscred.go`](../internal/api/harnesscred.go)) | Not applicable: the session is not recorded ([`internal/api/attach_session.go#runIsUnrecordable`](../internal/api/attach_session.go)) | The sandbox stops when idle; the stored capture ends as its row above says ([`internal/api/harnesscred.go`](../internal/api/harnesscred.go)) | There is no cast of what happened inside ([threat model](../threatmodel/THREAT-MODEL.md#51a-llm-egress-content-inspection--the-honest-claims-contract)) |
| `cloud_sts` grant ([POLICIES.md](POLICIES.md#eligible_grants--grantspec)) | Nowhere: the kind mints nothing in this release ([`internal/broker/broker.go#Broker.mintKind`](../internal/broker/broker.go)) | Nothing ([`internal/broker/broker.go#Broker.mintKind`](../internal/broker/broker.go)) | Nothing to read ([`internal/broker/broker.go#Broker.mintKind`](../internal/broker/broker.go)) | Not applicable ([`internal/api/runs_create_confinement.go#Server.resolveEnforcedConfinement`](../internal/api/runs_create_confinement.go)) | A policy carrying one is refused at run-create with `422` `run_grants_require_spire` ([`internal/api/runs_create_confinement.go#Server.resolveEnforcedConfinement`](../internal/api/runs_create_confinement.go)) | None ([`internal/api/runs_create_confinement.go#Server.resolveEnforcedConfinement`](../internal/api/runs_create_confinement.go)) |

- An egress redirect's token and a header-delivering integration are each authored as an `api_key` grant, so that row describes them ([`internal/api/artifact_redirect.go#Server.resolveRedirectToken`](../internal/api/artifact_redirect.go), [Nothing is ambient](operations/integrations.md#nothing-is-ambient)).
- The threat model's [resident-secret exceptions table](../threatmodel/THREAT-MODEL.md#51a-llm-egress-content-inspection--the-honest-claims-contract) is the authority for each exception's bounds. This page explains the shapes and links there.
- When a run ends, the kill-switch cascade stops further mints and audits each minted credential with what was and was not invalidated: see [the kill-switch cascade](../threatmodel/THREAT-MODEL.md#the-kill-switch-cascade-mechanically).

## Residuals

> [!WARNING]
> **An allowed destination can send an added credential back.** The proxy relays the destination's response to the sandbox as it arrives ([`internal/egress/proxy/local_routes.go#relay`](../internal/egress/proxy/local_routes.go)). A destination that echoes request headers therefore returns the value to the workload.

- This applies to every row above that says "No copy placed" or names a placeholder.
- Not holding the value also does not stop the workload from using it: a request it sends to that host carries the header.
- What bounds it:
  - The header goes to the exact host its rule names, and a host carries one credential ([`internal/egress/proxy/inject.go#buildInjector`](../internal/egress/proxy/inject.go)).
  - A member's inline `api_key` grant that names an operator's secret must match a pairing the operator listed, header and format included ([`internal/api/inline_policy_secrets.go#storedSecretPairingInCeiling`](../internal/api/inline_policy_secrets.go)).
  - A header secret in a component a person defines is sent over TLS only ([`internal/types/component.go#ComponentDefinition.Validate`](../internal/types/component.go)).
  - The value is on the run's masking record, so an echo printed to a masked output is replaced there.
- What does not bound it: masking does not remove the bytes from the response the workload receives.

> [!WARNING]
> **Masking matches exact bytes, and some paths are not masked.** The full statement is [Output masking, and the paths it does not cover](../threatmodel/THREAT-MODEL.md#41-output-masking-and-the-paths-it-does-not-cover).

- Masked: the value as registered. That is the raw secret, the formatted header value, and a file's value without its trailing line break ([`internal/egress/proxy/inject.go#registerHeaderCredential`](../internal/egress/proxy/inject.go), [`internal/api/runs_dispatch_files.go#fileSecretRenderings`](../internal/api/runs_dispatch_files.go)).
- Not masked: a base64, hex, split or otherwise rewritten form of the value ([`internal/secretmask/secretmask.go`](../internal/secretmask/secretmask.go)).
- Not masked: SSH exec, SFTP and direct-tcpip channels ([`internal/api/sshgateway_channels.go`](../internal/api/sshgateway_channels.go)); see [SSH.md](SSH.md#recording).
- Not masked: casts written through the optional recording mount set by `WARDYN_RECORDING_MOUNT` ([ENV.md](ENV.md)).
- Masking covers output Wardyn relays or stores. It does not cover what the workload sends to a destination its policy allows.

> [!WARNING]
> **A credential delivered inside can be read by anything running in that sandbox.** That includes code the workload downloads and runs.

- A variable is readable by processes running as the workload's user for the whole run ([`internal/types/types.go#GrantEnvSecret`](../internal/types/types.go)).
- A file is mode `0400` and owned by the workload's user on Docker ([`internal/runner/managed_files.go#ComponentSecretFileMode`](../internal/runner/managed_files.go)).
- On Kubernetes it is mode `0440` and readable by every process in the workload's pod ([`internal/runner/k8s/managed_files.go#agentSecretItemMode`](../internal/runner/k8s/managed_files.go)).
- An `ssh_key` grant that is not approval-gated is re-mintable by any process in the sandbox until the run ends, although its file is wiped after the clone ([`internal/egress/proxy/local_routes.go#Proxy.handleBrokerMint`](../internal/egress/proxy/local_routes.go)).
- There is no expiry and nothing to revoke, so rotating the secret at its issuer is the remedy after a suspected leak.
- What bounds it:
  - who may deliver one, as [A value the program reads itself](#a-value-the-program-reads-itself) states;
  - the run's egress policy, which limits where the value can be sent;
  - a component's grants being `owner_only`, so the value at risk is the launching person's own.

> [!WARNING]
> **An organisation's shared component secret is the operator's credential, used by a member's run.** A `shared` secret is read from the operator's namespace and nowhere else ([`internal/api/runs_scm.go#injectionGrantRead`](../internal/api/runs_scm.go)).

- Only an organisation's component can carry one, and only as a header ([`internal/types/component.go#validateComponentDelivery`](../internal/types/component.go)).
- An organisation's component is restricted from its creation: a person may attach it only once an allow row names its id, unless an administrator lifts that restriction ([`internal/api/capabilities.go#capComponent`](../internal/api/capabilities.go)).
- The attach check is [`internal/api/components_authz.go#Server.componentAttachRefusal`](../internal/api/components_authz.go); see [Capabilities](operations/capabilities.md#capabilities-what-one-member-or-one-group-may-do).
- The secret's name is withheld from members at these doors:
  - the component view shows hosts and delivery modes, not secret names ([`internal/api/components_project.go#orgComponentView`](../internal/api/components_project.go));
  - a refusal relayed into the sandbox does not name it ([`internal/api/injection.go#sinkSharedSecretRefused`](../internal/api/injection.go));
  - its `secret.read` audit row names the grant, not the secret ([`internal/api/injection.go#injectionRead.auditTarget`](../internal/api/injection.go)).
- The residual: the member's workload can send requests that carry the operator's credential to the component's host, and an echoing host returns the operator's value into the member's sandbox.

## Which should I use?

1. Prefer a credential the proxy adds: a model provider, an `api_key` grant, a header-delivery component, or a brokered Git lane.
2. Use variable or file delivery only when the program must hold the value itself.
3. For Git, choose a brokered lane before `ssh_key`: `github_token`, `git_pat` with its broker on, or the Azure DevOps lane.

Before delivering a secret inside a sandbox, check:

- Can the tool be pointed at a host and a header instead? If so, use `api_key` ([POLICIES.md](POLICIES.md#api_key)).
- Is the secret scoped to this one use, and can you rotate it at its issuer?
- Does the run's egress policy list only destinations you would trust with the value?
- Is the secret the launching person's own? Mark a policy grant `owner_only` so an operator secret of the same name is not used ([POLICIES.md](POLICIES.md#owner_only)).
- Does the workload run code you have not reviewed? If so, treat the secret as disclosed to that code.

Before adding a credential at the proxy, check:

- Does the destination have an endpoint that returns request headers? If so, treat the credential as readable by the workload.
- Is the credential scoped to what this run should do at that host? The workload can send any request the run's policy allows there.

## Related pages

- The exceptions, row by row, with their bounds: [threat model, section 5.1a](../threatmodel/THREAT-MODEL.md#51a-llm-egress-content-inspection--the-honest-claims-contract).
- What masking covers and what it does not: [threat model, section 4.1](../threatmodel/THREAT-MODEL.md#41-output-masking-and-the-paths-it-does-not-cover).
- Grant kinds, scopes and write-time rules: [POLICIES.md](POLICIES.md#eligible_grants--grantspec).
- The switches named on this page: [ENV.md](ENV.md).
- How secrets are stored, encrypted and rotated: [Secrets and keys](operations/secrets-and-keys.md).
- Each person's own Azure DevOps access: [AZURE-DEVOPS.md](AZURE-DEVOPS.md).
- SSH into a run, and what that path records and masks: [SSH.md](SSH.md#bounds).
