# Glossary: gateway, proxy and broker

Several parts of Wardyn, and of the systems around it, are called a gateway, a proxy or a broker. This table tells them apart: what each one is, where it runs, and the page that owns it.

## Which is which

| Term | What it is | Where it runs | Its page |
|---|---|---|---|
| Egress proxy (`wardyn-proxy`) | One per run, and the run's only path out. It enforces the allowlist, first-use approval and tool rules, adds credentials the sandbox never holds, and writes the decision log | Beside the run's sandbox, outside it | [Architecture](../ARCHITECTURE.md#wardyn-proxy-role) |
| AI gateway | A gateway you run beside Wardyn that serves model and MCP traffic for every caller. Wardyn can send it a short-lived token naming the run | Central: one gateway for many runs, reached through each run's egress proxy | [An external AI gateway](operations/ai-gateway.md) |
| Model gateway | A model gateway you run, added as a `custom_endpoint` provider. It does routing, fallbacks, rate limits and guardrails; Wardyn checks `allowed_models` before it | Central, like an AI gateway | [Model gateways and detectors](operations/model-gateways.md) |
| SSH gateway | wardynd's own SSH listener into a running sandbox's terminal, for the run's owner with a registered public key. Off unless `WARDYN_SSH_LISTEN` is set | In the control plane | [SSH gateway](SSH.md) |
| UI sandbox gateway | wardynd's relay of one declared port inside a sandbox, such as a code editor, to a browser on its own origin. Off unless `WARDYN_UI_SANDBOX_LISTEN` is set | In the control plane, on a second address | [UI sandboxes](UI-SANDBOXES.md) |
| Ingress | Your ingress controller or load balancer in front of the console and the API. The chart renders an Ingress, not a Kubernetes Gateway API route | In front of wardynd, outside Wardyn | [Console Ingress](../deploy/helm/wardyn/README.md#console-ingress) |
| Gateway Timeout (`504`) | The ingress's answer when its read timeout passes, not Wardyn's. A slow request, such as a live egress edit, can go on after it. Set the read timeout above the request's limit | The ingress | [Console Ingress](../deploy/helm/wardyn/README.md#console-ingress) |
| Trusted proxies | The ingress or load balancer peers `WARDYN_TRUSTED_PROXIES` lists, so the rate limits count each caller's own address | In front of wardynd | [Behind a shared ingress](operations/ingress-rate-limits.md) |
| Upstream proxy | Your corporate proxy, which every run's egress proxy dials through when the site config names one | Your network | [Network settings](OPERATIONS.md#network-upstream-proxy-and-egress-redirects) |
| Git broker | The egress proxy's route for a run's GitHub traffic. It adds a GitHub App installation token the control plane mints, so for a brokered repository the sandbox holds only the grant's id | Inside the egress proxy | [`github_token`](POLICIES.md#github_token) |
| PAT broker | The egress proxy's route that adds a stored `git_pat` token for another forge, so the token stays in the proxy's memory. With `WARDYN_GIT_PAT_BROKER=off`, an operator's run gets the token itself | Inside the egress proxy | [`git_pat`](POLICIES.md#git_pat) |
| Token broker | The control plane's issuer of a run's short-lived credentials. For a grant that requires approval, it mints only after the approval | In the control plane | [How a credential reaches a run](CREDENTIALS.md#every-kind-side-by-side) |
