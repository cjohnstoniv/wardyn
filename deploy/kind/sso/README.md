# Multi-user (SSO) overlay for the kind quickstart

Turns the single-command quickstart cluster into a multi-user install: Dex as
a demo identity provider with two static users, and the chart re-rendered on
OIDC instead of the admin token.

Everything here is demo-grade and public by design — the bcrypt literal is the
word `password`, the client secret is a fixed demo string (the same pair
`deploy/compose/dex.yaml` already ships). Do not reuse any of it outside a
demo cluster.

The whole overlay is one command — `make kind-sso` (and `make kind-sso-down`
to remove it), which is `overlay.sh` beside this file. It runs exactly the
sequence below plus a `kind load` of the locally built images, so the overlay
runs THIS tree's wardynd rather than whatever the quickstart loaded earlier:

```sh
make agent-images          # builds wardyn/agent-aws-sso:local, which the overlay loads
WARDYN_QUICKSTART_HTTP_PORT=8280 WARDYN_QUICKSTART_SSH_PORT=2322 make kind-quickstart
make kind-sso
```

Neither target creates or deletes a cluster: `make kind-quickstart` owns that,
and `make kind-sso-down` removes only the overlay's own objects.

By hand, it is:

```sh
# 1. The cluster (ports chosen to coexist with a compose stack on :8080)
WARDYN_QUICKSTART_HTTP_PORT=8280 WARDYN_QUICKSTART_SSH_PORT=2322 make kind-quickstart

# 2. Dex + the SSO overlay (the default policy rides along, floored to CC1 —
#    redundant since 0.7.8, when the baked default's own floor became CC1;
#    kept because it states this Fence-only cluster's floor explicitly rather
#    than inheriting whatever the image ships)
#    awsssofake.yaml rides along: fake AWS IAM Identity Center + a
#    bedrock-runtime stub, so the AWS SSO login and a per-user Bedrock run are
#    exercisable with no AWS tenant (docs/OPERATIONS.md, "Testing AWS SSO
#    without an AWS tenant"). It impersonates AWS with no signing at all —
#    throwaway clusters only.
kubectl --context kind-wardyn-quickstart apply -f deploy/kind/sso/dex.yaml
kubectl --context kind-wardyn-quickstart apply -f deploy/kind/sso/awsssofake.yaml
helm --kube-context kind-wardyn-quickstart upgrade wardyn deploy/helm/wardyn \
  -n wardyn --reuse-values -f deploy/kind/sso/values.yaml \
  --set-file defaultPolicy=deploy/kind/sso/default-policy.json

# 3. The browser-facing issuer (split-horizon: the cluster reaches Dex by its
#    Service; your browser reaches it here)
kubectl --context kind-wardyn-quickstart -n wardyn port-forward svc/wardyn-dex 5557:5556
```

To run a second SSO cluster beside the first, give it its own cluster name,
image tag and ports: `WARDYN_KIND_SSO_DEX_PORT` and `WARDYN_QUICKSTART_HTTP_PORT`
are rendered into `dex.yaml`'s issuer and callback and into the chart's OIDC
issuer and redirect at overlay time (the defaults leave the file unchanged).

Sign in at http://localhost:8280 — `admin@wardyn.local` / `password` is the
operator, `member@wardyn.local` / `password` a member (the chart's
`WARDYN_OIDC_ROLE_MAP` decides which is which).

## The AWS SSO walk

With the overlay up, `scripts/kind-sso-walk.sh` (needs `WARDYN_TEST_K8S=1`)
signs both principals in through Dex and proves the whole per-user AWS SSO path
against the fake: the member's own containerized `aws sso login`, a capture that
is theirs and not the admin's, and a Bedrock run whose role credentials the fake
confirms were minted for the MEMBER's pinned account/role. See the script's
header for the four preconditions — the fourth (`internal_hosts`) is the one
that fails first if forgotten.

## The Azure DevOps profile

`WARDYN_KIND_SSO_PROFILE=ado` swaps Dex for a fake Entra tenant and adds a fake
Azure DevOps, both served by one TEST image (`test/adofake/cmd`, built into
`wardyn/test-adofake:local` by `overlay.sh` and never published). It is a
separate profile, not a second issuer, because the per-person Azure DevOps
sign-in binds only to the console's OWN Entra app registration.

The fake is a forward proxy that terminates TLS for `login.microsoftonline.com`
and `dev.azure.com` with a walk CA the overlay mints once per cluster (Secret
`wardyn-test-adofake-ca`). wardynd reaches it as `WARDYN_DAEMON_PROXY_URL`, a
run's sidecar as site-config's `upstream_proxy_url`, and both trust the CA
through `trustedCA` — the corporate-proxy posture Wardyn already supports, so
no product code is re-pointed. Its `/authorize` is a picker with no password:
`admin@wardyn.test` and `member@wardyn.test`.

The profile builds `wardynd` and `wardyn-proxy` under its OWN tag (`kind-ado`,
`WARDYN_QUICKSTART_IMAGE_TAG`) and pins the release to it, so it never retags
the `:quickstart` images another cluster's overlay reloads. Give the quickstart
the same tag, as below.

```sh
WARDYN_QUICKSTART_CLUSTER=wardyn-ado WARDYN_QUICKSTART_IMAGE_TAG=kind-ado WARDYN_QUICKSTART_HTTP_PORT=8580 WARDYN_QUICKSTART_SSH_PORT=2522 make kind-quickstart
WARDYN_KIND_SSO_PROFILE=ado WARDYN_QUICKSTART_CLUSTER=wardyn-ado WARDYN_QUICKSTART_HTTP_PORT=8580 make kind-sso
WARDYN_TEST_K8S=1 WARDYN_KIND_SSO_PROFILE=ado WARDYN_QUICKSTART_CLUSTER=wardyn-ado scripts/kind-sso-walk.sh
```

The walk (`scripts/lib/kind-sso-walk-ado.sh`) signs the admin in before any
Azure DevOps row exists, has that session write the row, then signs the member
in. It asserts the member's login was widened and captured one blob, the
member's; that the member's exec run resolves its injection and reads REST and
runs `git ls-remote`; and that the fake's `/_seen` saw the member's subject and
never the admin's.
