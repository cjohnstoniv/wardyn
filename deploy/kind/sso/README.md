# Multi-user (SSO) overlay for the kind quickstart

Turns the single-command quickstart cluster into the multi-user install the
"One command to a cluster" episode films: Dex as a demo identity provider with
two static users, and the chart re-rendered on OIDC instead of the admin token.

Everything here is demo-grade and public by design — the bcrypt literal is the
word `password`, the client secret is a fixed demo string (the same pair
`deploy/compose/dex.yaml` already ships). Do not reuse any of it outside a
demo cluster.

```sh
# 1. The cluster (ports chosen to coexist with a compose stack on :8080)
WARDYN_QUICKSTART_HTTP_PORT=8280 WARDYN_QUICKSTART_SSH_PORT=2322 make kind-quickstart

# 2. Dex + the SSO overlay
kubectl --context kind-wardyn-quickstart apply -f deploy/kind/sso/dex.yaml
helm --kube-context kind-wardyn-quickstart upgrade wardyn deploy/helm/wardyn \
  -n wardyn --reuse-values -f deploy/kind/sso/values.yaml

# 3. The browser-facing issuer (split-horizon: the cluster reaches Dex by its
#    Service; your browser reaches it here)
kubectl --context kind-wardyn-quickstart -n wardyn port-forward svc/wardyn-dex 5557:5556
```

Sign in at http://localhost:8280 — `admin@wardyn.local` / `password` is the
operator, `member@wardyn.local` / `password` a member (the chart's
`WARDYN_OIDC_ROLE_MAP` decides which is which).
