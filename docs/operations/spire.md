> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# SPIRE identity (experimental)

- `WARDYN_IDENTITY=spire` makes each run token a SPIRE JWT-SVID minted by your SPIRE server.
- wardynd also registers one SPIRE entry per run, so the run's proxy pod, and only it, can be attested for an X.509-SVID carrying the run's SPIFFE ID.
- Experimental and off by default. Kubernetes only in this release: Docker and remote runners keep the bearer token.
- wardynd keeps each token's delegation chain (the person, the sponsor, the operator flag, the run and the expiry) in `identity_token_bindings`, one row per token. A SPIRE-signed token with no row is refused, so a JWT-SVID a workload fetched for itself is not a run token.
- The default provider, `embedded`, is unchanged.

## What you install

- The SPIRE server, the SPIRE agents and the SPIFFE CSI driver, from upstream. Wardyn bundles none of them. The provider is built against SPIRE 1.16.0 (upstream `spire` chart 0.31.0) but has not yet been run against a live SPIRE server.
- Node attestation by `k8s_psat`, so an agent proves which cluster node it runs on.
- A trust domain of Wardyn's own. As SPIRE admin, wardynd can mint any SVID in its trust domain, so share it with nothing else. Set `WARDYN_TRUST_DOMAIN` to it.
- An admin entry for wardynd, described [below](#the-admin-entry-for-wardynd).
- Then turn on the chart's `spire` block ([chart README](../../deploy/helm/wardyn/README.md#spire-identity-spireenabled)), which sets the variables below.

## Settings

| Variable | Chart value | Meaning |
| --- | --- | --- |
| `WARDYN_SPIRE_AGENT_SOCKET` | `spire.socketPath` | wardynd's Workload API, inside the CSI mount at `/spiffe-workload-api`. |
| `WARDYN_SPIRE_SERVER_ADDRESS` | `spire.server.address`, `.port` | The SPIRE server, over mTLS, as `spiffe://<trust domain>/spire/server`. |
| `WARDYN_SPIRE_PARENT_ID` | `spire.parentId` | The node alias that parents every run entry. |
| `WARDYN_SPIRE_SELECTORS` | rendered | The run-entry selectors, with a `{run_id}` placeholder. |
| `WARDYN_POD_SERVICE_ACCOUNT` | from the pod spec | wardynd's own service account. |
| `WARDYN_K8S_SPIFFE_CSI_DRIVER` | `spire.csiDriver` | The CSI driver each proxy pod mounts read-only at `/spiffe-workload-api`. |
| `WARDYN_K8S_SPIFFE_PROXY_SERVICE_ACCOUNT` | `spire.proxyServiceAccount` | The account each proxy pod runs as. The agent pod stays on `default`. |

- The two `WARDYN_K8S_SPIFFE_*` variables are set together or not at all; one alone refuses boot. `wardyn-runnerd` refuses either.
- Each proxy pod mounts the agent's socket at `/spiffe-workload-api/spire-agent.sock`; nothing in the proxy reads it yet. Keep that file name in the CSI volume for the release that does.

## Workload attestation selectors

- A run entry must carry each of these four selectors exactly once. Anything else refuses boot.

| Selector | Why it is there |
| --- | --- |
| `k8s:ns:<runs namespace>` | A pod in another namespace carrying the proxy's labels must not match. The value must equal `WARDYN_K8S_NAMESPACE`, which must be set. |
| `k8s:sa:<proxy account>` | The agent pod runs as `default` in the same namespace; the account tells the two apart. The value must equal `WARDYN_K8S_SPIFFE_PROXY_SERVICE_ACCOUNT` and is never `default`. |
| `k8s:pod-label:wardyn.component:proxy` | Only the proxy pod matches, never the agent pod. |
| `k8s:pod-label:wardyn.run-id:{run_id}` | Only this run's proxy matches. wardynd fills in the run id at the run's first mint. |

- A selector of another attestor type, `docker:` among them, refuses boot.
- A `k8s:ns:` or `k8s:sa:` value that is not byte-equal to its variable refuses boot. The message names the selector, the variable and both values, so a typo cannot register entries for another namespace.

## The parent ID

- `WARDYN_SPIRE_PARENT_ID` must select this cluster's agents only.
- Namespace and account names repeat across clusters, so a parent shared with a second cluster lets that cluster's pods match the run entries.
- Boot cannot check this. It is an operator rule.

## The admin entry for wardynd

- Its SPIFFE ID is `spiffe://<trust domain>/wardyn/control-plane`, and it carries `-admin`.
- It has exactly one `k8s:ns:` equal to wardynd's own namespace and exactly one `k8s:sa:` equal to wardynd's account (`WARDYN_POD_SERVICE_ACCOUNT`), and no selector of another attestor type.
- wardynd's account is neither the proxy account nor `default`.
- At boot wardynd lists every entry with that ID and refuses any other shape. A list error refuses boot too. The chart refuses equal accounts, and wardynd on `default`.
- Why: by default the runs namespace is the release namespace, where every proxy mounts the Workload API socket. An entry that selected on the namespace alone, or on the proxy's account, would hand a compromised proxy the admin SVID.

## Who may create proxy pods

- Creating a pod with the proxy's labels and the proxy's account in the runs namespace is the same as issuing a run identity.
- So grant `create pods` there, with that account, to admins only.
- SPIRE's Kubernetes workload attestor trusts a pod's cgroup membership only against unprivileged workloads.

## The workload claim

- A run's SPIFFE ID stays per run: `spiffe://<trust domain>/workload/<owner>/<name>/run/<run id>` for a workload's run, `spiffe://<trust domain>/agent-run/<run id>` for any other.
- The part up to `/run/` is a stable prefix a relying party may match. It is never issued alone, so per-run revocation and per-run audit hold.
- The run token's verified claims add a workload claim, `<owner>/<name>`: the same value the gateway token carries as `wardyn_workload`. It is read from the SPIRE-signed ID, and an ad-hoc run has none.
- wardynd sets it from the launch it admitted. It never comes from a request body, from the token's presenter or from the workload.
- A rule that must tell workloads apart compares the claim whole, never a full SPIFFE ID, which changes with every run.

## Lifetimes and revocation

| Bound | Value |
| --- | --- |
| wardynd's boot wait for its SVID, the trust bundle and the entry list | at most 10 s |
| A run's X.509-SVID (`x509_svid_ttl` on its entry) | 10 minutes |
| A run token | 1 hour |
| A run's lapse after its last token | 65 minutes |

- These are configured limits, not measurements.
- A kill writes the run to the deny-list first, then deletes its entry.
- A certificate is not revoked. A run's X.509-SVID can outlive the kill by up to its 10 minutes, so Wardyn never takes it alone as a run's authority: the bearer and the deny-list are checked on every request.
- The ground-truth sensor's token makes no entry.
- A mint that races a kill deletes the run's entry again and returns no token.
- A few cleanup paths revoke a run's identity on a best-effort basis and do not retry. When one fails, the run's entry stays until you remove it by hand. Its tokens stay refused, and no live pod carries its run label.
- To find such entries, list the entries under `WARDYN_SPIRE_PARENT_ID` that carry a `k8s:pod-label:wardyn.run-id:<run id>` selector, and delete any whose run has ended or no longer exists.

## Boot refusals

| Cause | What happens |
| --- | --- |
| A runner other than `k8s` | wardynd exits: "The spire identity provider needs the k8s runner in this release." |
| A missing variable, or the selector rules above | wardynd exits, naming the variable or selector. |
| No control-plane X.509-SVID, no JWT bundle, or no entry list within 10 s | wardynd exits with "did not answer within 10s" and the socket or server address. |
| The admin entry's shape, or `WARDYN_POD_SERVICE_ACCOUNT` empty, `default` or the proxy account | wardynd exits. |

## SPIRE server outage

- No run starts, and every renew fails. Verify keeps working from the cached trust bundle.
- Each running run lapses at its current token's expiry plus the lapse allowance. A proxy renews at most every 15 minutes and retries a failure every minute, so runs whose last renew landed just before an outage of under about 45 minutes survive it.
- A wardynd restart during the outage does not boot: the 10 s wait ends with "identity provider: spire identity: … did not answer within 10s", and wardynd exits. It never serves with a provider it could not check.
- While no replica is up there is no console, no API and no kill switch from Wardyn. Kubernetes restarts the pod on its back-off until SPIRE answers. Runs keep the tokens they hold and lapse as above.
- So do not restart wardynd while SPIRE is down, and run the SPIRE server and agents highly available, as upstream describes.
- To regain the console while SPIRE is down, set `WARDYN_IDENTITY=embedded` and turn `WARDYN_INTERNAL_MTLS` off if it is on, because `embedded` does not boot with it on. Every spire-minted token is then refused, so those runs stop at their next renew.
- The ground-truth sensor's token is minted through the same provider. Its rotator retries every minute; the sensor keeps the token it holds until it expires (1 hour), and its events are not ingested after that until a mint lands.

## Mutual TLS on the proxy hop

- `WARDYN_INTERNAL_MTLS=true` (off by default) makes each run's proxy present its run's X.509-SVID to wardynd's internal listener beside its run token. A request that did not come through a runner's relay is refused unless the certificate verifies against the SPIRE trust bundle and names the same run as the token ([ENV.md](../ENV.md#wardyn_internal_mtls)).
- wardynd exits at boot with it on unless `WARDYN_IDENTITY` is `spire`, `WARDYN_RUNNER` is `k8s` and `WARDYN_CONTROL_PLANE_URL` is `https://`.
- Runs whose requests come through a runner's relay are bound by that runner and present no certificate.
- The bearer and the denylist are always checked; a certificate never admits a request on its own.
- With it on, a run on an organization pool's remote-hosted runner is refused `internal_peer_required` at its proxy's first control-plane call, because that proxy has no run certificate. Estates with remote-hosted runners leave it off.
- A run already running when it is turned on has a proxy with no run certificate, so it is refused the same way.
- Certificates are not revoked at the kill: one lasts until it expires, and the denied run token is what stops the run.
- To turn it on, add it to the chart's `env`:

  ```yaml
  env:
    WARDYN_INTERNAL_MTLS: "true"
  ```

- The handshake is checked by unit tests in this release; it has not been run end to end on a cluster.

## Scope

- Kubernetes only in this release. Docker and remote runners keep the bearer token.
- The AI gateway's client certificate and token are unchanged under `spire` ([AI gateway](ai-gateway.md)).
