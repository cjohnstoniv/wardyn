> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Kubernetes: the agent-sandbox backend (experimental)

- An optional way to make each run's agent pod: a [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) controller creates it from a `Sandbox` object, not Wardyn directly.
- Off by default. Experimental. Server mode only.
- Turning it on trusts that controller with every run's pod. Read [what it widens](#what-turning-it-on-widens) first.

## What is claimed

- Experimental, off by default. Tested in CI on kind, Kubernetes v1.30.0 (kind v0.23.0, node image pinned by digest), Calico, confinement class CC1, with agent-sandbox v1.0.9 by digest, whose API is `v1beta1` (beta). Server mode only.
- The CI job `conformance-k8s-agent-sandbox` runs on train pull requests and pushes to main only. It is not a required check. Its cluster is its own: this page says nothing about the Kubernetes version of Wardyn's plain `conformance-k8s` job.
- The job checks:
  - the plain conformance suite plus the backend's own cases, with the backend on;
  - every decision of the two admission policies, on a real API server;
  - each verb in the chart's Role, with `kubectl auth can-i` under a separate account.
- The suite itself runs as the wardynd ServiceAccount, which is bound to cluster-admin. So the chart's Role is not proven end to end.
- Not tested:
  - a controller rewriting a pod for real (fake clients only);
  - RuntimeClass under real Kata;
  - any other controller version;
  - running under the chart's Role end to end (CI's account is cluster-admin; the verbs are checked with `kubectl auth can-i`).
- The CI job that backs this is new. Treat each tested claim as true only while that job is green on the release you run.

## How a run is made

1. Wardyn creates the drives, the two NetworkPolicies, the Secret and the proxy pod, in the same order as without this backend.
2. It builds the agent pod as usual, then creates a bare `Sandbox` with the pod's name. The template carries a scheduling gate, `wardyn.dev/verify`, so no container can start yet.
3. It waits for the controller's pod and reads it back. Any difference from the pod Wardyn asked for, or a gate that is already gone, refuses the run and deletes the Sandbox.
4. Only then does it remove the gate, with a patch pinned to the pod's UID and resourceVersion.
5. The rest is unchanged: exec, terminal, logs, recording, drives, and the per-run NetworkPolicies, which stay the only policies that select the pod.

## Install the controller

- Wardyn's chart never installs the controller or its CRD. You do, and only the core install.
- Do not install the extensions manifest. Its ClusterRole adds NetworkPolicy write rights cluster-wide, and templates, claims and warm pools are unsupported here.

1. Download `sandbox.yaml` from the agent-sandbox `v1.0.9` release.
2. Check its sha256 against the one that release publishes.
3. Replace the controller image tag with the digest: `registry.k8s.io/agent-sandbox/agent-sandbox-controller@sha256:ef1a1a3076afc996e711c44b388fbb7ffb7cd6858953d7e685166b299d77bd38`.
4. Apply it and wait for the controller to roll out.

- Wardyn is tested against v1.0.9 only. Its API is beta and has changed in patch releases. Another version is unsupported.
- Wardyn's boot refuses when the cluster does not serve `agents.x-k8s.io/v1beta1` `sandboxes`.

## Turn it on

| Setting | Value | Meaning |
| --- | --- | --- |
| `k8s.agentSandbox.enabled` | `false` by default | The switch (`WARDYN_K8S_AGENT_SANDBOX`). Needs `k8s.enabled=true` and an enforced NetworkPolicy. |
| `k8s.agentSandbox.controller.namespace` | `agent-sandbox-system` | The namespace the controller runs in. |
| `k8s.agentSandbox.controller.serviceAccount` | `agent-sandbox-controller` | The ServiceAccount it runs as. Change both values if you installed it elsewhere. |

- The chart refuses `k8s.allowUnenforcedNetPol`, `k8s.ackAmbientDefaultDeny`, and a controller identity equal to wardynd's own ServiceAccount.
- Needs Kubernetes 1.30 or later. Scheduling readiness and ValidatingAdmissionPolicy are both stable since v1.30 ([pod scheduling readiness](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-scheduling-readiness/), [ValidatingAdmissionPolicy](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/), read 2026-11-03).
- Off renders what the chart rendered before the key existed: no extra RBAC, policy, CRD or env.
- On adds `patch` on `pods` and `create`, `get`, `list`, `delete` on `sandboxes` to wardynd's Role in the runs namespace, and two admission policies. Nothing goes in a ClusterRole.
- At boot wardynd also asks the API server whether it holds each of those verbs, and refuses to boot if one is missing.

## What turning it on widens

| # | Widening | Size | What to do |
| --- | --- | --- | --- |
| 1 | The controller becomes a trusted actor. Its ClusterRole can create, delete, patch and watch pods, services and PVCs in every namespace. | It can create any pod, with any ServiceAccount. Pod Security Admission still applies. | Trust it, or leave this off. Install the core controller only. |
| 2 | wardynd gains `patch` on `pods` in the runs namespace, to remove the gate. | Small. wardynd already creates and deletes pods and NetworkPolicies there. | The patch carries the pod's UID and resourceVersion. |
| 3 | A second way to take a pod out of its NetworkPolicy: edit the Sandbox's template labels. The controller copies label edits onto the live pod. | The same power as `patch` on pods, but a different verb on a different resource. An existing RBAC review may not cover it. | Nobody else may write `sandboxes` in the runs namespace. Keep the admission policies on. |

- Widening 1 in plain words: a pod that runs outside any NetworkPolicy is not confined. The controller can make one.
- Wardyn's own verbs on Sandboxes are create, get, list and delete. It holds no update, patch, watch or deletecollection on them.
- A Sandbox holds what the pod holds: Secret references, no values. Anyone who can read `sandboxes` sees what a reader of `pods` sees.
- A Sandbox that is not Wardyn's but has its name or labels is not trusted: the create fails on a name collision, and the read-back checks the pod's owner.

## The admission policies

- With the switch on, the chart renders two `ValidatingAdmissionPolicy` objects with bindings for the runs namespace. Both fail closed.
- The gate policy selects pod creates by the controller's authenticated identity, which no admission hook can rewrite. It refuses a pod without the gate, without a `Sandbox` owner, or that names a node.
- The ungate policy selects an update of a pod that held the gate. Only wardynd's ServiceAccount may remove it, and not in the same write as a change to labels, annotations, owners, images, tolerations, node selector, affinity, the deadline or the node, or an added ephemeral container.
- What they do not cover:
  - they are right only while the two configured identities are right (the controller's and wardynd's);
  - they say nothing about a hook that is not subject to admission, such as a compromised API server;
  - they do not stop someone who can write `sandboxes` from editing a label, which is why widening 3 is a rule for your RBAC too.
- A create the policy refuses is retried by the controller. You see repeated `SandboxPodCreateFailed` events on the `Sandbox`, and the run ends with the controller's words.

## The gate claim

- The gate holds unless the controller or an admission hook removes it. The two policies now cover the create and the gate-removal hooks, as far as the two identities are right.
- If a pod is read back without the gate, Wardyn refuses, tears the run down and says the pod may have started. It never says "nothing ran" in that case.
- The NetworkPolicy exists before the pod is created, whatever happens to the gate.
- What CI shows is API order and first-exec behaviour, the same as the plain path. It is not a proof about how fast your CNI programs a policy.

## What the read-back proves

| It proves | It does not prove |
| --- | --- |
| At start, the pod the controller made matches what Wardyn asked for: containers, images, env, mounts, security context, resources, volumes, `hostAliases`, DNS, RuntimeClass, placement and the labels the NetworkPolicy selects. No extra container or volume. | Anything after the gate is removed. |
| The pod is still held when it is checked: gate present, no node, phase Pending. | That nothing edited a Sandbox or pod later. |

- Afterwards, Wardyn reads Status. Each of these ends the run at the next Status read:
  - the pod carries a gate again;
  - a label the NetworkPolicies select is missing or changed;
  - a container or init container runs a different image than the Sandbox template asked for;
  - the Sandbox's spec was edited after create (its generation is above 1);
  - the Sandbox was replaced by one with a different identity (the pod's controlling owner UID is not the Sandbox's), such as one deleted with the orphan policy and made again under the same name;
  - the Sandbox is being deleted, or is missing while the pod lives.
- Nothing watches between reads, so this is not a time bound.
- The run ends as failed, and the replaced-pod sentence below is recorded as the run's failure hint.
- Teardown finds the run's agent pod by its fixed name, not by a label, so a pod whose labels were edited is still deleted.
- A mutating webhook that injects a sidecar makes the read-back refuse the run, though the plain path accepts it. That is deliberate.
- A real controller rewrite is tested with fake clients only.

## What the person sees

| When | Sentence |
| --- | --- |
| The cluster changed the pod | This run did not start: the cluster's sandbox controller changed the pod Wardyn asked for (`<field>`). Nothing ran. Ask your administrator. |
| The pod was replaced after start | This run ended: its pod was replaced outside Wardyn, and Wardyn does not start a replacement. Files on your drives are kept. Start a new run. |
| A placement label is reserved | This placement uses a label reserved by the sandbox controller. |
| `linux_vm` | a linux_vm sandbox runs only on a registered runner that offers it |
| Under `wardyn-runnerd`, at boot | k8s: the agent-sandbox backend does not run under wardyn-runnerd; unset WARDYN_K8S_AGENT_SANDBOX |

## Switching it off with live runs

- Switching off does not stop existing pods. A run that already started keeps running, because Wardyn removed its gate. Off also removes the RBAC on Sandboxes, so Wardyn can no longer delete them.
1. Drain first: let runs end or stop them.
2. Confirm their Sandboxes are gone: `kubectl get sandboxes -l wardyn.managed=true` in the runs namespace.
3. Or delete the survivors: `kubectl delete sandboxes -l wardyn.managed=true`.

- A Sandbox left behind keeps the controller watching it. If its pod is ever replaced, the replacement carries the gate and stays held, so it never starts.

- Stop and kill delete the Sandbox before the pods, so the controller does not recreate the agent pod. A failed Sandbox delete returns an error and keeps the NetworkPolicies. The orphan sweep also lists Sandboxes.

## Not supported

| Not supported | What happens |
| --- | --- |
| Runner mode (`wardyn-runnerd`) | Refused at boot. |
| Templates, claims, warm pools | No code path. Install the core controller only. |
| Pause and resume | Never offered. Wardyn never suspends a Sandbox. |
| `linux_vm` | Refused as before. |
| Revive, restart with current limits | 409 `revive_unsupported`, as on the plain path. |
| Controller versions other than v1.0.9 | Unsupported. |

- Other limits of the Kubernetes substrate are in [Kubernetes: known gaps](kubernetes-known-gaps.md).
